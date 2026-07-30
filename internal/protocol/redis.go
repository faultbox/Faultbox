package protocol

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"
)

func init() {
	Register(&redisProtocol{})
}

type redisProtocol struct{}

func (p *redisProtocol) Name() string { return "redis" }

func (p *redisProtocol) Methods() []string {
	return []string{"get", "set", "del", "ping", "keys", "lpush", "rpush", "lrange", "incr", "command"}
}

func (p *redisProtocol) Healthcheck(ctx context.Context, addr string, timeout time.Duration) error {
	a := ParseAddr(addr)
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", a.HostPort, 2*time.Second)
		if err == nil {
			// Send PING, expect +PONG. A server started with --requirepass
			// answers -NOAUTH until authenticated, so a password-protected
			// Redis is only ready once AUTH has succeeded.
			conn.SetDeadline(time.Now().Add(2 * time.Second))
			reader := bufio.NewReader(conn)
			authErr := redisAuth(conn, reader, a.User, a.Password)
			if authErr == nil {
				fmt.Fprint(conn, "*1\r\n$4\r\nPING\r\n")
				line, _ := reader.ReadString('\n')
				conn.Close()
				if strings.HasPrefix(line, "+PONG") {
					return nil
				}
			} else {
				conn.Close()
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
	return fmt.Errorf("Redis healthcheck %q timed out after %s", a.HostPort, timeout)
}

// redisAuth sends AUTH when a password is configured. With no password it is a
// no-op, so an unprotected server is untouched.
//
// Two forms: AUTH <password> for the default user, AUTH <user> <password> for
// a Redis 6+ ACL user. Without this, every command against a server started
// with --requirepass came back "-NOAUTH Authentication required" — and since
// the failure lands in the step result rather than the connection, only a spec
// that asserts on the result would ever see it.
func redisAuth(conn net.Conn, reader *bufio.Reader, user, password string) error {
	if password == "" {
		return nil
	}
	if user != "" && user != "default" {
		fmt.Fprintf(conn, "*3\r\n$4\r\nAUTH\r\n$%d\r\n%s\r\n$%d\r\n%s\r\n",
			len(user), user, len(password), password)
	} else {
		fmt.Fprintf(conn, "*2\r\n$4\r\nAUTH\r\n$%d\r\n%s\r\n", len(password), password)
	}
	line, err := reader.ReadString('\n')
	if err != nil {
		return fmt.Errorf("redis AUTH: %w", err)
	}
	if !strings.HasPrefix(line, "+OK") {
		return fmt.Errorf("redis AUTH rejected: %s", strings.TrimSpace(line))
	}
	return nil
}

func (p *redisProtocol) ExecuteStep(ctx context.Context, addr, method string, kwargs map[string]any) (*StepResult, error) {
	a := ParseAddr(addr)
	conn, err := net.DialTimeout("tcp", a.HostPort, 5*time.Second)
	if err != nil {
		return &StepResult{Success: false, Error: err.Error()}, nil
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(10 * time.Second))

	reader := bufio.NewReader(conn)
	start := time.Now()

	user := getStringKwarg(kwargs, "user", a.User)
	password := getStringKwarg(kwargs, "password", a.Password)
	if err := redisAuth(conn, reader, user, password); err != nil {
		return &StepResult{
			Success:    false,
			Error:      err.Error(),
			DurationMs: time.Since(start).Milliseconds(),
		}, nil
	}

	var args []string
	switch method {
	case "get":
		args = []string{"GET", getStringKwarg(kwargs, "key", "")}
	case "set":
		args = []string{"SET", getStringKwarg(kwargs, "key", ""), getStringKwarg(kwargs, "value", "")}
	case "del":
		args = []string{"DEL", getStringKwarg(kwargs, "key", "")}
	case "ping":
		args = []string{"PING"}
	case "keys":
		pattern := getStringKwarg(kwargs, "pattern", "*")
		args = []string{"KEYS", pattern}
	case "lpush":
		args = []string{"LPUSH", getStringKwarg(kwargs, "key", ""), getStringKwarg(kwargs, "value", "")}
	case "rpush":
		args = []string{"RPUSH", getStringKwarg(kwargs, "key", ""), getStringKwarg(kwargs, "value", "")}
	case "lrange":
		key := getStringKwarg(kwargs, "key", "")
		start := getStringKwarg(kwargs, "start", "0")
		stop := getStringKwarg(kwargs, "stop", "-1")
		args = []string{"LRANGE", key, start, stop}
	case "incr":
		args = []string{"INCR", getStringKwarg(kwargs, "key", "")}
	case "command":
		// Raw command: command(cmd="SET", args=["key", "value"])
		cmd := getStringKwarg(kwargs, "cmd", "")
		args = []string{cmd}
		// args= is documented as a list, and a list is what specs pass. The
		// string branch is the single-argument shorthand; before the runtime
		// converted lists properly, it was the only branch that ever fired, so
		// the documented form silently dropped every argument.
		if rawArgs, ok := kwargs["args"]; ok {
			switch v := rawArgs.(type) {
			case string:
				args = append(args, v)
			case []any:
				for _, item := range v {
					args = append(args, fmt.Sprintf("%v", item))
				}
			}
		}
	default:
		return nil, fmt.Errorf("unsupported redis method %q", method)
	}

	// Send RESP array.
	if err := writeRESP(conn, args); err != nil {
		return &StepResult{
			Success:    false,
			Error:      fmt.Sprintf("write: %v", err),
			DurationMs: time.Since(start).Milliseconds(),
		}, nil
	}

	// Read RESP response.
	val, err := readRESP(reader)
	if err != nil {
		return &StepResult{
			Success:    false,
			Error:      fmt.Sprintf("read: %v", err),
			DurationMs: time.Since(start).Milliseconds(),
		}, nil
	}

	body, _ := json.Marshal(map[string]any{"value": val})
	return &StepResult{
		Body:       string(body),
		Success:    true,
		DurationMs: time.Since(start).Milliseconds(),
	}, nil
}

// writeRESP writes a RESP array command.
func writeRESP(conn net.Conn, args []string) error {
	var sb strings.Builder
	fmt.Fprintf(&sb, "*%d\r\n", len(args))
	for _, arg := range args {
		fmt.Fprintf(&sb, "$%d\r\n%s\r\n", len(arg), arg)
	}
	_, err := fmt.Fprint(conn, sb.String())
	return err
}

// readRESP reads a single RESP response value.
func readRESP(r *bufio.Reader) (any, error) {
	line, err := r.ReadString('\n')
	if err != nil {
		return nil, err
	}
	line = strings.TrimRight(line, "\r\n")

	if len(line) == 0 {
		return nil, fmt.Errorf("empty RESP response")
	}

	switch line[0] {
	case '+': // Simple string
		return line[1:], nil
	case '-': // Error
		return nil, fmt.Errorf("redis error: %s", line[1:])
	case ':': // Integer
		n, _ := strconv.ParseInt(line[1:], 10, 64)
		return n, nil
	case '$': // Bulk string
		n, _ := strconv.Atoi(line[1:])
		if n < 0 {
			return nil, nil // null bulk string
		}
		buf := make([]byte, n+2) // +2 for \r\n
		_, err := r.Read(buf)
		if err != nil {
			return nil, err
		}
		return string(buf[:n]), nil
	case '*': // Array
		n, _ := strconv.Atoi(line[1:])
		if n < 0 {
			return nil, nil
		}
		result := make([]any, n)
		for i := 0; i < n; i++ {
			result[i], err = readRESP(r)
			if err != nil {
				return nil, err
			}
		}
		return result, nil
	default:
		return line, nil
	}
}

package star

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/twmb/franz-go/pkg/kgo"
	"go.starlark.net/starlark"
)

// The helper startup/fetch gates are test controls, not SUT launch machinery.
// The runtime's actual tracker is registered before any client socket exists.
func TestKafkaProcessOwnershipHelper(t *testing.T) {
	if os.Getenv("FAULTBOX_KAFKA_PROCESS_HELPER") != "1" {
		t.Skip("subprocess helper")
	}
	input := bufio.NewReader(os.Stdin)
	if line, err := input.ReadString('\n'); err != nil || strings.TrimSpace(line) != "start" {
		t.Fatalf("startup gate: %q %v", line, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 35*time.Second)
	defer cancel()
	fetchGate := make(chan struct{})
	stop := make(chan struct{})
	var release sync.Once
	if os.Getenv("FAULTBOX_KAFKA_BLOCK_FETCH") != "1" {
		release.Do(func() { close(fetchGate) })
	}
	go func() {
		defer close(stop)
		for {
			line, err := input.ReadString('\n')
			if err != nil {
				return
			}
			switch strings.TrimSpace(line) {
			case "fetch":
				release.Do(func() { close(fetchGate) })
			case "stop":
				return
			}
		}
	}()
	var blocked sync.Once
	c, err := kgo.NewClient(kgo.SeedBrokers(os.Getenv("FAULTBOX_KAFKA_ADDR")), kgo.ClientID("identical-client-id"), kgo.ConsumerGroup(os.Getenv("FAULTBOX_KAFKA_GROUP")), kgo.ConsumeTopics("events"), kgo.ConsumeResetOffset(kgo.NewOffset().AtEnd()), kgo.DisableAutoCommit(), kgo.FetchMaxWait(40*time.Millisecond), kgo.Dialer(func(dialCtx context.Context, network, addr string) (net.Conn, error) {
		conn, err := (&net.Dialer{}).DialContext(dialCtx, network, addr)
		if err != nil {
			return nil, err
		}
		return &kafkaFirstFetchGate{Conn: conn, ctx: ctx, release: fetchGate, blocked: &blocked}, nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { cancel(); c.Close() }()
	fetches := c.PollRecords(ctx, 1)
	if err := fetches.Err(); err != nil {
		t.Fatal(err)
	}
	records := fetches.Records()
	if len(records) != 1 || records[0].Offset != 0 || string(records[0].Value) != "first-real-record" {
		t.Fatalf("first record: %+v", records)
	}
	if err := c.CommitRecords(ctx, records...); err != nil {
		t.Fatal(err)
	}
	fmt.Println("KAFKA_RECEIVED first-real-record offset=0")
	select {
	case <-stop:
	case <-ctx.Done():
		t.Fatal("parent did not release helper shutdown")
	}
}

type kafkaFirstFetchGate struct {
	net.Conn
	ctx      context.Context
	release  <-chan struct{}
	blocked  *sync.Once
	mu       sync.Mutex
	buffered []byte
}

func (c *kafkaFirstFetchGate) Write(data []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.buffered = append(c.buffered, data...)
	for len(c.buffered) >= 4 {
		size := int(binary.BigEndian.Uint32(c.buffered))
		if size < 4 || size > 64*1024*1024 {
			return 0, fmt.Errorf("bad test Kafka frame length %d", size)
		}
		if len(c.buffered) < size+4 {
			break
		}
		frame := c.buffered[:size+4]
		if binary.BigEndian.Uint16(frame[4:6]) == 1 {
			select {
			case <-c.release:
			default:
				c.blocked.Do(func() { fmt.Println("KAFKA_FETCH_BLOCKED") })
				select {
				case <-c.release:
				case <-c.ctx.Done():
					return 0, c.ctx.Err()
				}
			}
		}
		remaining := frame
		for len(remaining) > 0 {
			n, err := c.Conn.Write(remaining)
			if err != nil {
				return 0, err
			}
			if n == 0 {
				return 0, io.ErrShortWrite
			}
			remaining = remaining[n:]
		}
		c.buffered = c.buffered[size+4:]
	}
	return len(data), nil
}

type kafkaHelperOutput struct {
	mu                        sync.Mutex
	data                      bytes.Buffer
	blocked, received         chan struct{}
	blockedOnce, receivedOnce sync.Once
}

func (o *kafkaHelperOutput) Write(data []byte) (int, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	n, err := o.data.Write(data)
	text := o.data.String()
	if strings.Contains(text, "KAFKA_FETCH_BLOCKED\n") {
		o.blockedOnce.Do(func() { close(o.blocked) })
	}
	if strings.Contains(text, "KAFKA_RECEIVED first-real-record offset=0\n") {
		o.receivedOnce.Do(func() { close(o.received) })
	}
	return n, err
}
func (o *kafkaHelperOutput) String() string { o.mu.Lock(); defer o.mu.Unlock(); return o.data.String() }

type kafkaManagedHelper struct {
	cmd     *exec.Cmd
	input   io.WriteCloser
	output  *kafkaHelperOutput
	done    chan struct{}
	waitErr error
}

func startKafkaManagedHelper(t *testing.T, ctx context.Context, rt *Runtime, service, group, addr string, block bool) *kafkaManagedHelper {
	t.Helper()
	rt.services[service] = &ServiceDef{Name: service}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(ctx, executable, "-test.run=^TestKafkaProcessOwnershipHelper$", "-test.count=1")
	cmd.Env = append(os.Environ(), "FAULTBOX_KAFKA_PROCESS_HELPER=1", "FAULTBOX_KAFKA_ADDR="+addr, "FAULTBOX_KAFKA_GROUP="+group)
	if block {
		cmd.Env = append(cmd.Env, "FAULTBOX_KAFKA_BLOCK_FETCH=1")
	}
	output := &kafkaHelperOutput{blocked: make(chan struct{}), received: make(chan struct{})}
	cmd.Stdout, cmd.Stderr = output, output
	input, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	helper := &kafkaManagedHelper{cmd: cmd, input: input, output: output, done: make(chan struct{})}
	go func() { helper.waitErr = cmd.Wait(); close(helper.done) }()
	unregister, err := rt.registerProcess(service, cmd.Process.Pid)
	if err != nil {
		_ = cmd.Process.Kill()
		<-helper.done
		t.Fatal(err)
	}
	t.Cleanup(func() {
		defer unregister()
		_ = input.Close()
		_ = cmd.Process.Kill()
		select {
		case <-helper.done:
		case <-time.After(5 * time.Second):
			t.Error("Kafka helper did not stop")
		}
	})
	if _, err := io.WriteString(input, "start\n"); err != nil {
		t.Fatal(err)
	}
	return helper
}

func TestKafkaProcessOwnershipAcrossRealClients(t *testing.T) {
	for _, proxied := range []bool{false, true} {
		t.Run(fmt.Sprintf("proxy=%v", proxied), func(t *testing.T) {
			rt := New(testLogger())
			t.Cleanup(rt.cleanup)
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			port := freePort(t)
			if err := rt.LoadString("kafka_processes.star", fmt.Sprintf(`
load("@faultbox/mocks/kafka.star", "kafka")
bus=kafka.broker("bus", interface=interface("main","kafka",%d),topics={"events":[]})
`, port)); err != nil {
				t.Fatal(err)
			}
			if err := rt.startMockService(ctx, "bus", rt.services["bus"]); err != nil {
				t.Fatal(err)
			}
			addr := fmt.Sprintf("127.0.0.1:%d", port)
			if proxied {
				if err := rt.preStartProxies(ctx, "bus", rt.services["bus"]); err != nil {
					t.Fatal(err)
				}
				addr = rt.proxyMgr.GetProxyAddr("bus", "main")
			}
			a := startKafkaManagedHelper(t, ctx, rt, "consumer-a", "group-a", addr, false)
			b := startKafkaManagedHelper(t, ctx, rt, "consumer-b", "group-b", addr, true)
			waitEvent := func(kind, group, service string) Event {
				t.Helper()
				ticker := time.NewTicker(10 * time.Millisecond)
				defer ticker.Stop()
				for {
					for _, event := range rt.events.Events() {
						if event.Type == kind && event.Fields["group"] == group && event.Fields["source_service"] == service {
							return event
						}
					}
					select {
					case <-ticker.C:
					case <-ctx.Done():
						t.Fatalf("missing %s for %s/%s\nA:%s\nB:%s\nevents:%+v", kind, group, service, a.output.String(), b.output.String(), rt.events.Events())
						return Event{}
					}
				}
			}
			assignedB := waitEvent("mock.kafka.assign", "group-b", "consumer-b")
			select {
			case <-b.output.blocked:
			case <-ctx.Done():
				t.Fatalf("B never blocked its first Fetch: %s", b.output.String())
			}
			readyA := waitEvent("mock.kafka.group_ready", "group-a", "consumer-a")
			for _, event := range rt.events.Events() {
				if event.Type == "mock.kafka.group_ready" && event.Fields["group"] == "group-b" {
					t.Fatalf("A's Fetch incorrectly readied B before B sent any Fetch: %+v", event)
				}
			}
			if _, err := io.WriteString(b.input, "fetch\n"); err != nil {
				t.Fatal(err)
			}
			readyB := waitEvent("mock.kafka.group_ready", "group-b", "consumer-b")
			for _, pair := range []struct {
				event Event
				child *kafkaManagedHelper
			}{{readyA, a}, {readyB, b}} {
				if pair.event.Service != "bus" || pair.event.Fields["source_pid"] != strconv.Itoa(pair.child.cmd.Process.Pid) || pair.event.Fields["source_instance"] == "" || pair.event.Fields["attribution"] != "process_instance" || pair.event.Fields["offset"] != "0" {
					t.Fatalf("missing proven readiness: %+v", pair.event)
				}
			}
			if readyA.Fields["source_instance"] == readyB.Fields["source_instance"] || readyB.Fields["source_instance"] != assignedB.Fields["source_instance"] {
				t.Fatal("process instance identity changed or conflated")
			}
			thread := &starlark.Thread{Name: "test"}
			thread.SetLocal("faultbox.context", ctx)
			ref := &InterfaceRef{Service: rt.services["bus"], Interface: rt.services["bus"].Interfaces["main"], runtime: rt}
			for _, service := range []string{"consumer-a", "consumer-b"} {
				_, err := rt.executeStep(thread, ref, "wait_ready", nil, []starlark.Tuple{{starlark.String("topics"), starlark.NewList([]starlark.Value{starlark.String("events")})}, {starlark.String("service"), starlark.String(service)}})
				if err != nil {
					t.Fatalf("public readiness for %s: %v", service, err)
				}
			}
			receipt, err := rt.executeStep(thread, ref, "publish", nil, []starlark.Tuple{{starlark.String("topic"), starlark.String("events")}, {starlark.String("data"), starlark.String("first-real-record")}})
			if err != nil || !receipt.(*Response).Ok {
				t.Fatalf("publish: %v %v", receipt, err)
			}
			for _, item := range []struct {
				group, service string
				ready          Event
				child          *kafkaManagedHelper
			}{{"group-a", "consumer-a", readyA, a}, {"group-b", "consumer-b", readyB, b}} {
				if _, err := rt.executeStep(thread, ref, "wait_committed", starlark.Tuple{receipt}, []starlark.Tuple{{starlark.String("service"), starlark.String(item.service)}}); err != nil {
					t.Fatalf("public commit barrier for %s: %v", item.service, err)
				}
				commit := waitEvent("mock.kafka.commit", item.group, item.service)
				if commit.Service != "bus" || commit.Fields["offset"] != "1" || commit.Fields["source_pid"] != item.ready.Fields["source_pid"] || commit.Fields["source_instance"] != item.ready.Fields["source_instance"] {
					t.Fatalf("commit ownership mismatch: %+v", commit)
				}
				select {
				case <-item.child.output.received:
				case <-ctx.Done():
					t.Fatalf("helper did not receive first record: %s", item.child.output.String())
				}
				if _, err := io.WriteString(item.child.input, "stop\n"); err != nil {
					t.Fatal(err)
				}

				select {
				case <-item.child.done:
					if item.child.waitErr != nil {
						t.Fatalf("helper failed: %v\n%s", item.child.waitErr, item.child.output.String())
					}
				case <-ctx.Done():
					t.Fatalf("helper shutdown timed out: %s", item.child.output.String())
				}
			}
		})
	}
}

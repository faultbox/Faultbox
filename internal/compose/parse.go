// Package compose parses docker-compose.yml files and generates Faultbox specs.
package compose

import (
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// ComposeFile represents a docker-compose.yml.
type ComposeFile struct {
	Services map[string]ComposeService `yaml:"services"`
}

// ComposeService represents a single service in docker-compose.yml.
type ComposeService struct {
	Image       string            `yaml:"image"`
	Build       interface{}       `yaml:"build"` // string or struct
	Ports       []string          `yaml:"ports"`
	DependsOn   interface{}       `yaml:"depends_on"` // []string or map
	Environment interface{}       `yaml:"environment"` // []string or map
	Healthcheck *ComposeHealth    `yaml:"healthcheck"`
	Command     interface{}       `yaml:"command"`
	Volumes     []string          `yaml:"volumes"`
}

// ComposeHealth represents a healthcheck in docker-compose.yml.
type ComposeHealth struct {
	Test     interface{} `yaml:"test"` // string or []string
	Interval string      `yaml:"interval"`
	Timeout  string      `yaml:"timeout"`
	Retries  int         `yaml:"retries"`
}

// Service is a parsed service ready for spec generation.
type Service struct {
	Name        string
	Image       string
	Protocol    string
	Port        int
	DependsOn   []string
	Env         map[string]string
	Healthcheck string
}

// Parse reads a docker-compose.yml and returns parsed services.
func Parse(path string) ([]Service, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}

	var cf ComposeFile
	if err := yaml.Unmarshal(data, &cf); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}

	if len(cf.Services) == 0 {
		return nil, fmt.Errorf("no services found in %s", path)
	}

	var services []Service
	for name, cs := range cf.Services {
		svc := Service{
			Name:  sanitizeName(name),
			Image: cs.Image,
			Env:   parseEnv(cs.Environment),
		}

		if svc.Image == "" {
			svc.Image = name + ":latest"
		}

		// Parse ports — take the first exposed port.
		if len(cs.Ports) > 0 {
			svc.Port = parsePort(cs.Ports[0])
		}

		// Detect protocol from image name or port.
		svc.Protocol = detectProtocol(svc.Image, svc.Port)

		// If no port found, infer from protocol.
		if svc.Port == 0 {
			svc.Port = defaultPort(svc.Protocol)
		}

		// Parse depends_on.
		svc.DependsOn = parseDependsOn(cs.DependsOn)

		// Generate healthcheck.
		svc.Healthcheck = generateHealthcheck(svc.Protocol, svc.Port)

		services = append(services, svc)
	}

	// Sort by dependency order.
	services = topoSort(services)

	return services, nil
}

// GenerateSpec produces a .star file from parsed services.
func GenerateSpec(services []Service) string {
	var sb strings.Builder

	sb.WriteString("# faultbox.star — generated from docker-compose.yml\n")
	sb.WriteString("#\n")
	sb.WriteString("# Run:   faultbox test faultbox.star\n")
	sb.WriteString("# JSON:  faultbox test faultbox.star --format json\n\n")

	// Service declarations.
	svcMap := make(map[string]bool)
	for _, svc := range services {
		svcMap[svc.Name] = true
	}

	for _, svc := range services {
		iface := interfaceName(svc.Protocol)
		sb.WriteString(fmt.Sprintf("%s = service(\"%s\",\n", svc.Name, svc.Name))
		sb.WriteString(fmt.Sprintf("    image=\"%s\",\n", svc.Image))
		sb.WriteString(fmt.Sprintf("    interface(\"%s\", \"%s\", %d),\n", iface, svc.Protocol, svc.Port))

		// Environment.
		if len(svc.Env) > 0 {
			sb.WriteString("    env={")
			keys := sortedKeys(svc.Env)
			for i, k := range keys {
				if i > 0 {
					sb.WriteString(", ")
				}
				sb.WriteString(fmt.Sprintf("%q: %q", k, svc.Env[k]))
			}
			sb.WriteString("},\n")
		}

		// depends_on.
		if len(svc.DependsOn) > 0 {
			var deps []string
			for _, d := range svc.DependsOn {
				if svcMap[d] {
					deps = append(deps, d)
				}
			}
			if len(deps) > 0 {
				sb.WriteString(fmt.Sprintf("    depends_on=[%s],\n", strings.Join(deps, ", ")))
			}
		}

		// Healthcheck.
		sb.WriteString(fmt.Sprintf("    healthcheck=%s,\n", svc.Healthcheck))
		sb.WriteString(")\n\n")
	}

	// Happy path test.
	sb.WriteString("# --- Tests ---\n\n")

	// Find the "main" service (has depends_on, or http/grpc protocol).
	mainSvc := findMainService(services)

	sb.WriteString("def test_happy_path():\n")
	sb.WriteString("    \"\"\"Verify all services start and respond.\"\"\"\n")
	for _, svc := range services {
		iface := interfaceName(svc.Protocol)
		switch svc.Protocol {
		case "http":
			sb.WriteString(fmt.Sprintf("    resp = %s.%s.get(path=\"/\")\n", svc.Name, iface))
			sb.WriteString(fmt.Sprintf("    assert_true(resp.status < 500, \"%s should respond\")\n", svc.Name))
		case "postgres":
			sb.WriteString(fmt.Sprintf("    resp = %s.%s.query(\"SELECT 1\")\n", svc.Name, iface))
			sb.WriteString(fmt.Sprintf("    assert_true(resp.status == \"ok\", \"%s should respond\")\n", svc.Name))
		case "redis":
			sb.WriteString(fmt.Sprintf("    resp = %s.%s.command(\"PING\")\n", svc.Name, iface))
			sb.WriteString(fmt.Sprintf("    assert_eq(resp, \"PONG\")\n"))
		case "tcp":
			sb.WriteString(fmt.Sprintf("    resp = %s.%s.send(data=\"PING\")\n", svc.Name, iface))
			sb.WriteString(fmt.Sprintf("    assert_true(len(resp) > 0, \"%s should respond\")\n", svc.Name))
		default:
			sb.WriteString(fmt.Sprintf("    # TODO: add health check for %s (%s)\n", svc.Name, svc.Protocol))
		}
	}
	sb.WriteString("\nscenario(test_happy_path)\n")

	// Fault test for the main service.
	if mainSvc != nil {
		sb.WriteString(fmt.Sprintf(`
# --- Fault scenarios ---

def test_write_failure():
    """What happens when %s can't write?"""
    def scenario():
        # TODO: exercise the service and check error handling
        pass
    fault(%s, write=deny("EIO"), run=scenario)
`, mainSvc.Name, mainSvc.Name))
	}

	return sb.String()
}

// --- Helpers ---

// Well-known port → protocol mapping.
var portProtocol = map[int]string{
	5432:  "postgres",
	3306:  "mysql",
	6379:  "redis",
	27017: "mongodb",
	9092:  "kafka",
	4222:  "nats",
	5672:  "amqp",
	11211: "memcached",
	80:    "http",
	443:   "http",
	8080:  "http",
	8000:  "http",
	3000:  "http",
	9090:  "http",
	50051: "grpc",
}

// Image name → protocol mapping.
var imageProtocol = map[string]string{
	"postgres":  "postgres",
	"mysql":     "mysql",
	"mariadb":   "mysql",
	"redis":     "redis",
	"valkey":    "redis",
	"mongo":     "mongodb",
	"mongodb":   "mongodb",
	"kafka":     "kafka",
	"nats":      "nats",
	"rabbitmq":  "amqp",
	"memcached": "memcached",
	"nginx":     "http",
	"envoy":     "http",
	"traefik":   "http",
}

func detectProtocol(image string, port int) string {
	// Check image name first.
	img := strings.ToLower(image)
	for prefix, proto := range imageProtocol {
		if strings.Contains(img, prefix) {
			return proto
		}
	}
	// Check port.
	if proto, ok := portProtocol[port]; ok {
		return proto
	}
	// Default to http.
	if port > 0 {
		return "http"
	}
	return "tcp"
}

func defaultPort(protocol string) int {
	for port, proto := range portProtocol {
		if proto == protocol {
			return port
		}
	}
	return 8080
}

func interfaceName(protocol string) string {
	switch protocol {
	case "http":
		return "http"
	case "grpc":
		return "grpc"
	case "postgres":
		return "pg"
	case "mysql":
		return "mysql"
	case "redis":
		return "redis"
	case "kafka":
		return "kafka"
	case "nats":
		return "nats"
	case "amqp":
		return "amqp"
	case "mongodb":
		return "mongo"
	case "memcached":
		return "memcached"
	default:
		return "main"
	}
}

func generateHealthcheck(protocol string, port int) string {
	switch protocol {
	case "http":
		return fmt.Sprintf(`http("localhost:%d/health")`, port)
	case "postgres", "mysql", "redis", "kafka", "nats", "amqp", "mongodb", "memcached":
		return fmt.Sprintf(`tcp("localhost:%d")`, port)
	case "grpc":
		return fmt.Sprintf(`tcp("localhost:%d")`, port)
	default:
		return fmt.Sprintf(`tcp("localhost:%d")`, port)
	}
}

func parsePort(portSpec string) int {
	// Handle "8080:8080", "8080:8080/tcp", "8080"
	parts := strings.Split(portSpec, ":")
	var portStr string
	if len(parts) == 2 {
		portStr = parts[1]
	} else {
		portStr = parts[0]
	}
	portStr = strings.Split(portStr, "/")[0] // strip /tcp, /udp
	port, _ := strconv.Atoi(portStr)
	return port
}

func parseDependsOn(raw interface{}) []string {
	switch v := raw.(type) {
	case []interface{}:
		var deps []string
		for _, d := range v {
			if s, ok := d.(string); ok {
				deps = append(deps, sanitizeName(s))
			}
		}
		return deps
	case map[string]interface{}:
		var deps []string
		for k := range v {
			deps = append(deps, sanitizeName(k))
		}
		sort.Strings(deps)
		return deps
	}
	return nil
}

func parseEnv(raw interface{}) map[string]string {
	env := make(map[string]string)
	switch v := raw.(type) {
	case []interface{}:
		for _, item := range v {
			if s, ok := item.(string); ok {
				parts := strings.SplitN(s, "=", 2)
				if len(parts) == 2 {
					env[parts[0]] = parts[1]
				}
			}
		}
	case map[string]interface{}:
		for k, val := range v {
			env[k] = fmt.Sprintf("%v", val)
		}
	}
	return env
}

func sanitizeName(name string) string {
	return strings.ReplaceAll(strings.ReplaceAll(name, "-", "_"), ".", "_")
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func findMainService(services []Service) *Service {
	// Service with most dependencies is likely the "app".
	var best *Service
	bestDeps := -1
	for i := range services {
		if len(services[i].DependsOn) > bestDeps {
			bestDeps = len(services[i].DependsOn)
			best = &services[i]
		}
	}
	// Fallback: first HTTP service.
	if best == nil || bestDeps == 0 {
		for i := range services {
			if services[i].Protocol == "http" {
				return &services[i]
			}
		}
	}
	return best
}

// topoSort orders services so dependencies come first.
func topoSort(services []Service) []Service {
	nameMap := make(map[string]*Service)
	for i := range services {
		nameMap[services[i].Name] = &services[i]
	}

	visited := make(map[string]bool)
	var result []Service
	var visit func(name string)
	visit = func(name string) {
		if visited[name] {
			return
		}
		visited[name] = true
		if svc, ok := nameMap[name]; ok {
			for _, dep := range svc.DependsOn {
				visit(dep)
			}
			result = append(result, *svc)
		}
	}

	// Visit in alphabetical order for determinism.
	names := make([]string, 0, len(services))
	for _, s := range services {
		names = append(names, s.Name)
	}
	sort.Strings(names)
	for _, name := range names {
		visit(name)
	}
	return result
}

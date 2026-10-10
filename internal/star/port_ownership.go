package star

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/faultbox/Faultbox/internal/portowner"
	"strings"
	"time"
)

type portConflict struct {
	service, network, host string
	port                   int
	cause                  error
}

func (e *portConflict) Error() string {
	return fmt.Sprintf("service %q fixed host port %d is already in use: %v", e.service, e.port, e.cause)
}
func (e *portConflict) Unwrap() error { return e.cause }

type processOrigin struct{ start, service, test string }

func (rt *Runtime) explainPortConflict(err error) error {
	var conflict *portConflict
	if !errors.As(err, &conflict) {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	owners, lookupErr := portowner.Lookup(ctx, conflict.network, conflict.host, conflict.port)
	notes := []string{}
	rt.processOriginMu.Lock()
	for i := range owners {
		owner := &owners[i]
		if origin, ok := rt.processOrigins[owner.PID]; ok && origin.start != "" && origin.start == owner.Start {
			owner.Service, owner.Test = origin.service, origin.test
		}
		label := fmt.Sprintf("pid=%d command=%q", owner.PID, owner.Command)
		if owner.Service != "" {
			label += fmt.Sprintf(" service=%q test=%q", owner.Service, owner.Test)
		}
		notes = append(notes, label)
	}
	rt.processOriginMu.Unlock()
	fields := map[string]string{"port": fmt.Sprint(conflict.port), "network": conflict.network, "host": conflict.host}
	if len(owners) > 0 {
		data, _ := json.Marshal(owners)
		fields["owners"] = string(data)
	}
	if lookupErr != nil {
		fields["ownership_error"] = lookupErr.Error()
		notes = append(notes, lookupErr.Error())
	}
	rt.events.recordOnly("port_in_use", conflict.service, fields)
	return codedf(CodePortInUse, "%v; listener ownership: %s", conflict, strings.Join(notes, "; "))
}
func (rt *Runtime) waitServicePortsReleased(ctx context.Context, svc *ServiceDef) error {
	tick := time.NewTicker(20 * time.Millisecond)
	defer tick.Stop()
	for {
		err := checkFixedServicePorts(svc)
		if err == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("ports not released: %w (%v)", rt.explainPortConflict(err), ctx.Err())
		case <-tick.C:
		}
	}
}

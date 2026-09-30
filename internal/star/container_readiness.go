package star

import (
	"context"
	"fmt"
	"time"
)

func (rt *Runtime) waitContainerReady(ctx context.Context, name, id, check string, timeout time.Duration) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	health := make(chan error, 1)
	go func() {
		defer func() {
			if recovered := recover(); recovered != nil {
				health <- fmt.Errorf("healthcheck panic: %v", recovered)
			}
		}()
		health <- waitReady(ctx, check, timeout)
	}()
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	inspect := func() error {
		exited, code, err := rt.dockerClient.ContainerExit(ctx, id)
		if err != nil {
			return err
		}
		if exited {
			return startupFailure(name, code, nil)
		}
		return nil
	}
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case err := <-health:
			if err != nil {
				return err
			}
			return inspect()
		case <-ticker.C:
			if err := inspect(); err != nil {
				return err
			}
		}
	}
}

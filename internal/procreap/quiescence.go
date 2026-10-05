package procreap

import (
	"context"
	"fmt"
	"time"
)

// Quiesce is the preservation-only form of Sweep. Unlike best-effort cleanup,
// failure to read the table or confirm a signalled writer's exit is an error.
func Quiesce(ctx context.Context, opts Options) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	victims, err := Sweep(opts)
	if err != nil {
		return err
	}
	timer := time.NewTimer(time.Second)
	defer timer.Stop()
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		live := false
		for _, v := range victims {
			if processAliveFunc(v.PID) {
				live = true
				break
			}
		}
		if !live {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
			return fmt.Errorf("run writers remained alive after termination")
		case <-tick.C:
		}
	}
}

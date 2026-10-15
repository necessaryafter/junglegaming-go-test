package worker

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestBackgroundStartsAndJoinsTasks(t *testing.T) {
	tests := []struct {
		name  string
		tasks int
	}{
		{name: "no tasks"},
		{name: "one component", tasks: 1},
		{name: "four independent components", tasks: 4},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			startupCanceled := make(chan struct{})
			started := make(chan struct{}, tt.tasks)
			finished := make(chan struct{}, tt.tasks)
			process := &background{}

			for range tt.tasks {
				process.tasks = append(process.tasks, func(ctx context.Context) {
					<-startupCanceled

					if err := ctx.Err(); err != nil {
						t.Errorf("component inherited canceled startup context: %v", err)
					}

					started <- struct{}{}
					<-ctx.Done()
					finished <- struct{}{}
				})
			}

			startup, cancel := context.WithCancel(t.Context())
			if err := process.start(startup); err != nil {
				cancel()
				t.Fatal(err)
			}

			cancel()
			close(startupCanceled)
			t.Cleanup(func() {
				ctx, cancel := context.WithTimeout(context.Background(), time.Second)
				defer cancel()

				if err := process.stop(ctx); err != nil {
					t.Errorf("join tasks during cleanup: %v", err)
				}
			})

			ctx, cancel := context.WithTimeout(t.Context(), time.Second)
			defer cancel()

			for range tt.tasks {
				select {
				case <-started:
				case <-ctx.Done():
					t.Fatal("component did not start")
				}
			}

			if len(finished) != 0 {
				t.Fatal("startup context cancellation stopped a running component")
			}

			if err := process.stop(ctx); err != nil {
				t.Fatal(err)
			}

			if len(finished) != tt.tasks {
				t.Fatalf("finished tasks = %d, want %d before shutdown returns", len(finished), tt.tasks)
			}
		})
	}
}

func TestBackgroundShutdownRespectsContext(t *testing.T) {
	tests := []struct {
		name    string
		context func() (context.Context, context.CancelFunc)
		want    error
	}{
		{
			name: "canceled shutdown",
			context: func() (context.Context, context.CancelFunc) {
				ctx, cancel := context.WithCancel(context.Background())
				cancel()

				return ctx, cancel
			},
			want: context.Canceled,
		},
		{
			name: "expired shutdown deadline",
			context: func() (context.Context, context.CancelFunc) {
				return context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
			},
			want: context.DeadlineExceeded,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			release := make(chan struct{})
			process := &background{tasks: []func(context.Context){
				func(context.Context) { <-release },
			}}
			if err := process.start(t.Context()); err != nil {
				t.Fatal(err)
			}

			t.Cleanup(func() {
				close(release)
				ctx, cancel := context.WithTimeout(context.Background(), time.Second)
				defer cancel()

				if err := process.stop(ctx); err != nil {
					t.Errorf("join released component: %v", err)
				}
			})

			ctx, cancel := tt.context()
			defer cancel()

			if err := process.stop(ctx); !errors.Is(err, tt.want) {
				t.Fatalf("shutdown error = %v, want %v", err, tt.want)
			}
		})
	}
}

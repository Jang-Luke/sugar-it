package cli

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"sugarintegration/internal/control"
	"sugarintegration/internal/transfer"
	"sugarintegration/internal/vfs"
)

const pollInterval = 250 * time.Millisecond

func (e *env) cpCmd(use, op string) *cobra.Command {
	var onConflict string
	var detach bool
	cmd := &cobra.Command{
		Use:   use + " <src> <dst> [--on-conflict fail|skip|overwrite|rename] [--detach]",
		Short: strings.ToUpper(op[:1]) + op[1:] + " files between hosts",
		Args:  cobra.ExactArgs(2),
		RunE: plain(func(cmd *cobra.Command, args []string) error {
			switch onConflict {
			case "", "fail", "skip", "overwrite", "rename":
			default:
				return usagef("--on-conflict must be fail, skip, overwrite or rename, not %q", onConflict)
			}
			for _, a := range args {
				if _, err := target(a, true); err != nil {
					return err
				}
			}
			c, err := e.client()
			if err != nil {
				return err
			}
			job, err := c.StartJob(cmd.Context(), control.TransferRequest{Op: op, Src: args[0], Dst: args[1], OnConflict: onConflict})
			if err != nil {
				return err
			}
			if detach {
				fmt.Fprintln(e.stdout, job.ID)
				return nil
			}
			return e.wait(cmd.Context(), c, job)
		}),
	}
	cmd.Flags().StringVar(&onConflict, "on-conflict", "fail", "what to do when the destination exists: fail, skip, overwrite or rename")
	cmd.Flags().BoolVar(&detach, "detach", false, "print the job ID and return at once")
	return cmd
}

func terminal(s transfer.State) bool {
	switch s {
	case transfer.StateDone, transfer.StatePartial, transfer.StateFailed, transfer.StateCanceled:
		return true
	}
	return false
}

// wait follows job until it ends and returns the exit status. Ctrl-C stops
// only the waiting.
func (e *env) wait(ctx context.Context, c *control.Client, job transfer.Job) error {
	tty := isTTY(e.stderr)
	lastLine := time.Time{}
	progressed := false
	for {
		if terminal(job.State) {
			if tty && progressed {
				fmt.Fprint(e.stderr, "\r\033[K")
			}
			return e.finish(job)
		}
		line := progressLine(job)
		if tty {
			fmt.Fprintf(e.stderr, "\r\033[K%s", line)
			progressed = true
		} else if time.Since(lastLine) >= 5*time.Second {
			fmt.Fprintln(e.stderr, line)
			lastLine = time.Now()
		}
		select {
		case <-ctx.Done():
			if tty && progressed {
				fmt.Fprint(e.stderr, "\r\033[K")
			}
			fmt.Fprintf(e.stderr, "stopped waiting; job %s keeps running in the background.\ncancel it with: sugar-it jobs cancel %s\n", job.ID, job.ID)
			return exitWith(exitFailure)
		case <-time.After(pollInterval):
		}
		next, err := c.Job(ctx, job.Host, job.ID)
		if err != nil {
			if ctx.Err() != nil {
				continue
			}
			return err
		}
		job = next
	}
}

func progressLine(j transfer.Job) string {
	pct := 0
	if j.TotalBytes > 0 {
		pct = int(100 * j.DoneBytes / j.TotalBytes)
	}
	s := fmt.Sprintf("%s %s: %d%% (%s of %s, %d/%d files)", j.ID, j.State, pct,
		humanSize(j.DoneBytes), humanSize(j.TotalBytes), j.DoneFiles, j.TotalFiles)
	if j.BytesPerSec > 0 {
		s += " " + humanSize(j.BytesPerSec) + "/s"
	}
	return s
}

// finish reports a finished job and picks the exit code: 0 only for done.
func (e *env) finish(j transfer.Job) error {
	fmt.Fprintf(e.stderr, "%s: %s, %d of %d files", j.ID, j.State, j.DoneFiles, j.TotalFiles)
	if j.SkippedFiles > 0 {
		fmt.Fprintf(e.stderr, ", %d skipped", j.SkippedFiles)
	}
	if j.FailedFiles > 0 {
		fmt.Fprintf(e.stderr, ", %d failed", j.FailedFiles)
	}
	fmt.Fprintln(e.stderr)
	if j.State == transfer.StateDone {
		return nil
	}
	if j.Error != nil {
		fmt.Fprintf(e.stderr, "  %s\n", itemText(*j.Error))
	}
	for _, f := range j.Failures {
		fmt.Fprintf(e.stderr, "  %s\n", itemText(f))
	}
	if j.FailedFiles > len(j.Failures) {
		fmt.Fprintf(e.stderr, "  ... and %d more\n", j.FailedFiles-len(j.Failures))
	}
	return exitWith(exitFailure)
}

func itemText(f transfer.ItemError) string {
	if f.Path == "" {
		return f.Code + ": " + f.Message
	}
	return f.Path + ": " + f.Code + ": " + f.Message
}

func (e *env) jobsCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "jobs", Short: "Follow and control transfers", Args: cobra.ArbitraryArgs, RunE: group}
	cmd.AddCommand(
		&cobra.Command{
			Use: "list", Short: "List transfers", Args: cobra.NoArgs,
			RunE: e.run(func(ctx context.Context, c *control.Client, _ []string) error {
				jobs, err := c.Jobs(ctx)
				if err != nil {
					return err
				}
				return e.printJobs(jobs)
			}),
		},
		&cobra.Command{
			Use: "show <id>", Short: "Show one transfer", Args: cobra.ExactArgs(1),
			RunE: e.run(func(ctx context.Context, c *control.Client, args []string) error {
				j, err := findJob(ctx, c, args[0])
				if err != nil {
					return err
				}
				if e.json {
					return printJSON(e.stdout, j)
				}
				fmt.Fprintf(e.stdout, "%s\n", progressLine(j))
				fmt.Fprintf(e.stdout, "%s %s:%s -> %s:%s\n", j.Spec.Op, j.Spec.Src.Share, j.Spec.Src.Path, j.Spec.Dst.Share, j.Spec.Dst.Path)
				if j.Error != nil {
					fmt.Fprintf(e.stdout, "error: %s\n", itemText(*j.Error))
				}
				for _, f := range j.Failures {
					fmt.Fprintf(e.stdout, "failed: %s\n", itemText(f))
				}
				for _, f := range j.Skipped {
					fmt.Fprintf(e.stdout, "skipped: %s\n", itemText(f))
				}
				return nil
			}),
		},
		&cobra.Command{
			Use: "cancel <id>", Short: "Cancel a transfer", Args: cobra.ExactArgs(1),
			RunE: e.run(func(ctx context.Context, c *control.Client, args []string) error {
				j, err := findJob(ctx, c, args[0])
				if err != nil {
					return err
				}
				return c.CancelJob(ctx, j.Host, j.ID)
			}),
		},
		&cobra.Command{
			Use: "retry <id>", Short: "Retry the failed parts of a transfer", Args: cobra.ExactArgs(1),
			RunE: e.run(func(ctx context.Context, c *control.Client, args []string) error {
				j, err := findJob(ctx, c, args[0])
				if err != nil {
					return err
				}
				return c.RetryJob(ctx, j.Host, j.ID)
			}),
		},
		&cobra.Command{
			Use: "watch [<id>]", Short: "Follow one transfer until it ends, or all transfers", Args: cobra.MaximumNArgs(1),
			RunE: e.run(func(ctx context.Context, c *control.Client, args []string) error {
				if len(args) == 1 {
					j, err := findJob(ctx, c, args[0])
					if err != nil {
						return err
					}
					return e.wait(ctx, c, j)
				}
				for {
					jobs, err := c.Jobs(ctx)
					if err != nil {
						if ctx.Err() != nil {
							return nil
						}
						return err
					}
					for _, j := range jobs {
						if !terminal(j.State) {
							fmt.Fprintln(e.stdout, progressLine(j))
						}
					}
					select {
					case <-ctx.Done():
						return nil
					case <-time.After(time.Second):
					}
				}
			}),
		},
	)
	return cmd
}

// findJob looks an ID up among all jobs; the host it runs on is part of the job.
func findJob(ctx context.Context, c *control.Client, id string) (transfer.Job, error) {
	jobs, err := c.Jobs(ctx)
	if err != nil {
		return transfer.Job{}, err
	}
	for _, j := range jobs {
		if j.ID == id {
			return j, nil
		}
	}
	return transfer.Job{}, vfs.E(vfs.CodeNotFound, "no job %q", id)
}

func (e *env) printJobs(jobs []transfer.Job) error {
	if jobs == nil {
		jobs = []transfer.Job{}
	}
	if e.json {
		return printJSON(e.stdout, jobs)
	}
	tw := newTable(e.stdout)
	fmt.Fprintln(tw, "ID\tOP\tSTATE\tPROGRESS\tFILES\tFAILED")
	for _, j := range jobs {
		pct := 0
		if j.TotalBytes > 0 {
			pct = int(100 * j.DoneBytes / j.TotalBytes)
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%d%%\t%d/%d\t%d\n", j.ID, j.Spec.Op, j.State, pct, j.DoneFiles, j.TotalFiles, j.FailedFiles)
	}
	return tw.Flush()
}

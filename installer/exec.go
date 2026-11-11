package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// Logger receives the lines a step prints.
type Logger interface {
	Logf(format string, args ...any)
}

type stderrLogger struct{}

func (stderrLogger) Logf(format string, args ...any) { fmt.Fprintf(os.Stderr, format+"\n", args...) }

// runCmd runs a command, sends its output to the log and returns an error that carries the last lines.
func runCmd(log Logger, timeout time.Duration, name string, args ...string) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Env = append(os.Environ(), "DEBIAN_FRONTEND=noninteractive", "LC_ALL=C")
	stdout, _ := cmd.StdoutPipe()
	cmd.Stderr = cmd.Stdout
	log.Logf("$ %s %s", name, strings.Join(args, " "))
	if err := cmd.Start(); err != nil {
		return err
	}
	var tail []string
	var mu sync.Mutex
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		sc := bufio.NewScanner(io.Reader(stdout))
		sc.Buffer(make([]byte, 64*1024), 1024*1024)
		for sc.Scan() {
			line := sc.Text()
			log.Logf("  %s", line)
			mu.Lock()
			tail = append(tail, line)
			if len(tail) > 6 {
				tail = tail[1:]
			}
			mu.Unlock()
		}
	}()
	wg.Wait()
	if err := cmd.Wait(); err != nil {
		mu.Lock()
		defer mu.Unlock()
		return fmt.Errorf("%s: %w: %s", name, err, strings.Join(tail, " | "))
	}
	return nil
}

func quietCmd(name string, args ...string) (string, error) {
	out, err := exec.Command(name, args...).CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

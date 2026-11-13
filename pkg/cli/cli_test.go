package cli

import (
	"bytes"
	"context"
	"io"
	"strings"
	"testing"
)

func TestHelpDoesNotExecuteCommands(t *testing.T) {
	for _, args := range [][]string{nil, {"help"}, {"--help"}, {"-h"}, {"help", "start"}, {"serve", "--help"}, {"start", "-h"}, {"status", "--help"}, {"reboot", "--help"}} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			called := false
			root := newCommand(func(context.Context, string, string, io.Writer) error { called = true; return nil })
			var output bytes.Buffer
			root.SetOut(&output)
			root.SetErr(&output)
			root.SetArgs(append([]string{"--config", "/nonexistent/lab.json"}, args...))
			if err := root.Execute(); err != nil {
				t.Fatal(err)
			}
			if called {
				t.Fatal("help executed a VM command")
			}
			if !strings.Contains(output.String(), "Usage:") || !strings.Contains(output.String(), "--config") {
				t.Fatal(output.String())
			}
		})
	}
}

func TestCommandDispatchAndConfigFlags(t *testing.T) {
	for _, tc := range []struct {
		args          []string
		command, path string
	}{
		{[]string{"serve"}, "serve", "lab.json"},
		{[]string{"--config", "custom.json", "start"}, "start", "custom.json"},
		{[]string{"status", "-c", "custom.json"}, "status", "custom.json"},
		{[]string{"reboot", "--config=custom.json"}, "reboot", "custom.json"},
	} {
		t.Run(strings.Join(tc.args, " "), func(t *testing.T) {
			calls := 0
			root := newCommand(func(_ context.Context, name, path string, _ io.Writer) error {
				calls++
				if name != tc.command || path != tc.path {
					t.Fatalf("got %s %s", name, path)
				}
				return nil
			})
			root.SetArgs(tc.args)
			if err := root.Execute(); err != nil {
				t.Fatal(err)
			}
			if calls != 1 {
				t.Fatalf("executed %d commands", calls)
			}
		})
	}
}

func TestInvalidArgumentsDoNotExecuteCommands(t *testing.T) {
	for _, args := range [][]string{{"strat"}, {"start", "extra"}, {"serve", "--unknown"}, {"status", "--config"}, {"reboot", "--", "-config"}} {
		called := false
		root := newCommand(func(context.Context, string, string, io.Writer) error { called = true; return nil })
		root.SetArgs(args)
		if err := root.Execute(); err == nil {
			t.Fatalf("accepted %v", args)
		}
		if called {
			t.Fatalf("invalid args executed command: %v", args)
		}
	}
}

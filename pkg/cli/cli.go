package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/Saumya40-codes/virutal-lab/pkg/controller"
	"github.com/Saumya40-codes/virutal-lab/pkg/firecracker"
	"github.com/spf13/cobra"
)

func execute(ctx context.Context, command, path string, out io.Writer) error {
	c, err := controller.LoadConfig(path)
	if err != nil {
		return err
	}
	if command == "serve" {
		return controller.Serve(ctx, c)
	}
	// Only fall back for a genuinely absent or stale controller endpoint.
	// Permission errors must not accidentally launch a second VM.
	conn, dialErr := net.DialTimeout("unix", c.ControlSocket(), time.Second)
	if dialErr == nil {
		conn.Close()
		client := firecracker.New(c.ControlSocket(), nil)
		client.HTTP.Timeout = 15 * time.Second
		defer client.Close()
		method, path := http.MethodGet, "/vm"
		if command == "start" {
			method, path = http.MethodPut, "/vm/start"
		}
		if command == "reboot" {
			method, path = http.MethodPut, "/vm/reboot"
		}
		data, err := client.Do(ctx, method, path, nil)
		if err != nil {
			return err
		}
		return printJSON(out, data)
	}
	if !errors.Is(dialErr, os.ErrNotExist) && !errors.Is(dialErr, syscall.ECONNREFUSED) {
		return fmt.Errorf("controller connection: %w", dialErr)
	}
	if command == "start" {
		return controller.Start(ctx, c)
	}
	// Preserve status/reboot access to a manually launched Firecracker instance.
	client := firecracker.New(c.Socket, nil)
	defer client.Close()
	if command == "status" {
		data, err := client.Do(ctx, http.MethodGet, "/", nil)
		if err != nil {
			return err
		}
		return printJSON(out, data)
	}
	_, err = client.Do(ctx, http.MethodPut, "/actions", map[string]string{"action_type": "SendCtrlAltDel"})
	if err == nil {
		slog.New(slog.NewJSONHandler(os.Stderr, nil)).With("vm_id", c.ID).Info("guest_reboot_requested", "shutdown_complete", false)
	}
	return err
}

func printJSON(out io.Writer, data []byte) error {
	var pretty bytes.Buffer
	if err := json.Indent(&pretty, data, "", "  "); err != nil {
		return err
	}
	_, err := fmt.Fprintln(out, pretty.String())
	return err
}

type runner func(context.Context, string, string, io.Writer) error

func Run(args []string) error {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	root := NewCommand()
	root.SetArgs(args)
	return root.ExecuteContext(ctx)
}

// NewCommand constructs an independent command tree without loading config or
// connecting to the controller. Help and completion never start infrastructure.
func NewCommand() *cobra.Command { return newCommand(execute) }

func newCommand(run runner) *cobra.Command {
	var configPath string
	root := &cobra.Command{
		Use:           "lab",
		Short:         "Manage the virtual infrastructure lab",
		Long:          "Manage Firecracker microVMs and local observability.",
		Args:          cobra.NoArgs,
		SilenceErrors: true,
		SilenceUsage:  true,
		RunE:          func(cmd *cobra.Command, _ []string) error { return cmd.Help() },
	}
	root.PersistentFlags().StringVarP(&configPath, "config", "c", "lab.json", "Configuration file; paths resolve relative to this file")
	for _, spec := range []struct{ name, short, long, example string }{
		{"serve", "Run the controller and metrics endpoint", "Run a persistent controller without booting a guest. Use another terminal to start the VM. Its console attaches here; the controller stays available after guest exit.", "  lab serve\n  lab serve --config lab.json"},
		{"start", "Boot the configured microVM", "Ask the running controller to boot the configured VM. Without a controller, run one VM in the foreground and exit when it stops.", "  lab start\n  lab start -c lab.json"},
		{"status", "Show the current VM state", "Query the controller for VM lifecycle state, or Firecracker directly if no controller is running.", "  lab status"},
		{"reboot", "Request a guest reboot", "Request guest reboot through virtual Ctrl+Alt+Del. Guest handling determines shutdown behavior. Firecracker exits on reset; serve mode keeps the controller running.", "  lab reboot"},
	} {
		root.AddCommand(&cobra.Command{
			Use: spec.name, Short: spec.short, Long: spec.long, Example: spec.example,
			Args: cobra.NoArgs,
			RunE: func(cmd *cobra.Command, _ []string) error {
				return run(cmd.Context(), cmd.Name(), configPath, cmd.OutOrStdout())
			},
		})
	}
	return root
}

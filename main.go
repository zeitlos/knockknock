package knockknock

import (
	"log/slog"
	"os"

	"github.com/zeitlos/knockknock/config"
	"github.com/zeitlos/knockknock/ipc"
	"github.com/zeitlos/knockknock/supervisor"
)

var ipcClient *ipc.Client

func Client() *ipc.Client {
	if ipcClient == nil {
		slog.Error("ipc client not initalized")
		os.Exit(1)
	}

	return ipcClient
}

func Run(config *config.Config, userMain func()) {
	var err error

	socketPath := supervisor.SocketPath()

	// Check if we're the supervisor or the child
	if supervisor.IsSupervisorProcess() {
		slog.Info("running as supervisor", "pid", os.Getpid(), "version", config.Version, "installationDir", config.BinaryDir)

		sv, err := supervisor.New(config)

		if err != nil {
			slog.Error("failed to initalize supervisor", "error", err)
			os.Exit(1)
		}

		slog.Info("starting ipc server", "socket", socketPath)

		server, err := ipc.NewIPCServer(sv)

		if err != nil {
			slog.Error("supervisor failed to initalize ipc server", "error", err)
			os.Exit(1)
		}

		server.Serve()
		defer server.Close()

		sv.Run()
		return
	}

	// We're the child - run user code with basic panic recovery
	slog.Info("running as child", "pid", os.Getpid(), "socket", socketPath, "version", config.Version)

	ipcClient, err = ipc.NewClient(socketPath)

	if err != nil {
		slog.Error("failed to initalize ipc client", "error", err)
		os.Exit(1)
	}

	runAsChild(userMain)
}

func runAsChild(userMain func()) {
	// Basic panic recovery for Go panics
	defer func() {
		if r := recover(); r != nil {
			slog.Error("Panic recovered: %v", r)
			os.Exit(1) // Signal crash to supervisor
		}
	}()

	// Run user's actual code
	userMain()

	// Clean exit
	os.Exit(0)
}

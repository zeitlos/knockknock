# knockknock

**Who's there? Self-updating Go binaries.**

A self-updating supervisor for Go binaries. knockknock wraps your application and handles the entire update lifecycle—from downloading new versions from OCI registries to performing atomic updates via symlinks, with automatic rollbacks if the new version crashes.

## Features

- **OCI registry integration**: Distribute binaries using the same infrastructure as your container images
- **Atomic updates**: Symlink swaps ensure no undefined states during updates
- **Automatic rollbacks**: Crashes trigger automatic rollback to the previous version
- **Version management**: Multiple versions stored on disk for easy rollback
- **IPC communication**: Clean separation between application and update logic
- **No external dependencies**: Everything lives in one binary

## Usage

```go
package main

import (
	"github.com/zeitlos/knockknock"
	"github.com/zeitlos/knockknock/config"
)

var Version = "0.0.1"

func main() {
	knockknock.Run(
		config.New("myapp").
			WithRepo("ghcr.io/myorg/myapp").
			WithVersion(Version),
		run,
	)
}

func run() {
	// Your application code here
}
```

### Checking for updates

```go
update, versions, err := knockknock.Client().CheckForUpdate(r.Context())

if err != nil {
	slog.Error("failed to check for updates", "error", err)
}
```

### Triggering an update

```go
if err := knockknock.Client().Update(context.Background(), selectedVersion); err != nil {
	slog.Error("failed to update", "error", err)
}
```

## Publishing Updates

New versions of the binary are published to an OCI compliant registry using ORAS. See [publish.sh](example/publish.sh) as a reference. Once published the new version will be picked up by knockknock.

## How it works

1. Your application receives an update request (via gRPC, HTTP, or any other mechanism)
2. It calls `knockknock.Client().Update()` to forward the request via Unix socket IPC
3. knockknock downloads the new version from an OCI registry using ORAS
4. It creates a backup symlink to the current version
5. It atomically swaps `/opt/<app-name>/current` to point to the new version
6. It stops your application, causing the process manager to restart it with the new binary


## Automatic Rollbacks

knockknock monitors the child process lifecycle. If your application crashes repeatedly (e.g., 5 times in short succession), it automatically rolls back to the previous version. No manual intervention required.

## Architecture

```
process manager (e.g. systemd)
  └─ knockknock (supervisor, listens on Unix socket)
       └─ myapp (child process)
```

knockknock runs as the main process managed by your process manager. Your application runs as a child process and communicates with knockknock through a Unix socket. This separation means:

- Your application code stays focused on its core responsibilities
- Only one process handles downloads and binary management
- Updates are orchestrated safely without race conditions
- Clean shutdowns and restarts are guaranteed

## License

MIT

## Credits

Built for the [flex.plane](https://flexplane.io) virtualization platform as a solution to managing distributed Go binary updates at scale.

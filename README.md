# FFmpeg Sidecar for Gotedo Impress

This repository contains the **FFmpeg Sidecar for Gotedo Impress**—a high-performance media processing microservice designed to interface directly with custom-built FFmpeg dynamic libraries.

To maintain compliance with the **GPL/LGPL license requirements** while keeping the parent application closed-source, this sidecar runs as an isolated background process and communicates with the main application strictly over network boundaries (via **gRPC** or **WebSockets**). Consequently, this sidecar and its complete build system are fully open-source.

---

## Architecture & Compliance

* **Process Isolation:** The sidecar links directly (and dynamically) to our custom-built FFmpeg libraries.
* **Network Boundary:** Your proprietary application communicates with this sidecar using gRPC or WebSockets. Because they run in separate execution domains and share no memory, your main application remains 100% proprietary and compliant under GPL/LGPL rules.
* **Reproducible Builds:** This repository includes the exact `Dockerfile` and compiler configuration flags (`builder.sh`) used to build FFmpeg, fulfilling the "Corresponding Source" requirements of copyleft licenses.

---

## Prerequisites

Before building, ensure your host system has the following installed:

1. **Docker** (Desktop or Engine) with **Buildx support** enabled.
2. **Go (v1.18 or later)** (to compile the task runner locally).
3. **Hardware virtualization** enabled in your BIOS/OS (required by Docker).

---

## Quick Start: Build FFmpeg

The build system is entirely self-contained. It compiles the task runner locally so you do not have to install global build utilities on your system.

### 1. Initialize & Install the Build Runner

From the root of this repository, run the following command to download and compile the `task` utility locally inside your workspace:

```bash
# 1. Initialize the Go module (if not already done)
go mod tidy

# 2. Compile and install 'task' into your local project directory
GOBIN="$(pwd)/bin" go install github.com/go-task/task/v3/cmd/task@latest

```

*(This compiles the `task` executable and places it securely under `./bin/` which is ignored by Git).*

### 2. Compile FFmpeg (All Targets)

To trigger the complete cross-compilation pipeline (this will pull the toolchains, run an APT caching proxy to speed up dependencies, and build FFmpeg for Linux, macOS, and Windows):

```bash
./bin/task build:ffmpeg

```

---

## Customizing the Build Target

By default, running the build task compiles libraries for all supported platforms: `windows,linux,darwin` across `amd64,arm64` architectures.

You can target a specific operating system and architecture by passing environmental overrides to the task runner:

### Build for Windows AMD64 Only

```bash
GOOS_VAR="windows" ARCH_VAR="amd64" ./bin/task build:ffmpeg

```

### Build for Apple Silicon (macOS M-series) Only

```bash
GOOS_VAR="darwin" ARCH_VAR="arm64" ./bin/task build:ffmpeg

```

### Build Multiple Selected Platforms

```bash
GOOS_VAR="linux,windows" ARCH_VAR="amd64" ./bin/task build:ffmpeg

```

---

## Build Artifacts Directory

Once a build successfully finishes, compiled outputs, header files, and shared binaries are deposited into the local `dist/` directory structured by platform:

```text
dist/
├── linux/
│   ├── amd64/          # Shared .so libraries and headers
│   └── arm64/
├── darwin/
│   ├── amd64/          # Shared .dylib libraries and headers
│   └── arm64/
└── windows/
    └── amd64/          # Shared .dll binaries, .lib files, and headers

```

---

## Cleaning Build Caches

The compilation environment caches heavy dependencies (like toolchains and package managers) to keep subsequent builds extremely fast. If you need to wipe these caches and force a completely clean build, run:

```bash
# Wipe the build caching structures
rm -rf build_cache/ dist/

```

---

## Licensing

The code in this repository (the sidecar interface, wrappers, and build configurations) is open-source. The compiled binaries produced by this build system link against **FFmpeg**, which is licensed under the **GNU Lesser General Public License (LGPL) v2.1** or the **GNU General Public License (GPL) v3** depending on compilation flags used (e.g. `--enable-gpl`).

By keeping this sidecar repository open-source and providing full build instructions (via the local `Dockerfile` and `Taskfile.yml`), we fully satisfy our open-source compliance commitments.
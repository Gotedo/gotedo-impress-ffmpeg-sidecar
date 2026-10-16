# Video conversion E2E tests

The tests build and call the native FFmpeg library through the sidecar. They do not invoke the FFmpeg command line program. Set `FFMPEG_CONVERSION_E2E=1` to enable the real media and real sidecar cases.

## Windows

Run these commands from the repository root in PowerShell on the Windows machine. Use the project's supported LLVM-MinGW toolchain on `PATH`, and make sure the Windows FFmpeg and libsql archives exist under `libs/windows/amd64`.

```powershell
task build:ffmpeg_sidecar TARGET_OS=windows TARGET_ARCH=amd64

$env:FFMPEG_CONVERSION_E2E = '1'
$env:GOTEDO_TEST_MODE = '1'
$env:GOTEDO_FFMPEG_SIDECAR_PATH = (Resolve-Path '.\pkg\ffmpeg\sidecar_bin\ffmpeg_sidecar_windows_amd64.exe').Path

Push-Location '.\sidecars\ffmpeg_sidecar'
go test -v -count=1 -run '^TestIntegration_(.*Video|CancelLongHEVCConversion)' .
Pop-Location

go test -tags=sqlite_fts5,development -v -count=1 ./pkg/impress_app `
  -run 'TestAutoVideoConversionEligibility|TestAutoVideoConversionBackfillE2E|TestVideoConversionQueueRecoveryAndCancellation|TestVideoConversionQueueRejectsWorkAtCapacity|TestVideoConversionServiceUsesRealSidecarProcess|TestVideoConversionDeduplicatesAndReturnsFreshCache|TestVideoConversionRetriesThenFails|TestVideoConversionCancelsRunningJobE2E'
```

The sidecar conversion tests print the selected H.264 encoder in their progress stream assertions and fail if the RPC does not report it. The app E2E tests exercise the durable libsql queue and launch the built sidecar executable.

## macOS ARM64

Build the sidecar from the current source, then run the same tests. `GOTEDO_FFMPEG_SIDECAR_USE_TCP=1` selects loopback TCP for tests running in restricted environments that deny Unix-domain socket creation; the app uses Unix-domain sockets by default.

```sh
task build:ffmpeg_sidecar TARGET_OS=darwin TARGET_ARCH=arm64
export FFMPEG_CONVERSION_E2E=1 GOTEDO_TEST_MODE=1 GOTEDO_FFMPEG_SIDECAR_USE_TCP=1
export GOTEDO_FFMPEG_SIDECAR_PATH="$PWD/pkg/ffmpeg/sidecar_bin/ffmpeg_sidecar_darwin_arm64"

(cd sidecars/ffmpeg_sidecar && go test -v -count=1 -run '^TestIntegration_(.*Video|CancelLongHEVCConversion)' .)
go test -tags=sqlite_fts5,development -v -count=1 ./pkg/impress_app \
  -run 'TestAutoVideoConversionEligibility|TestAutoVideoConversionBackfillE2E|TestVideoConversionQueueRecoveryAndCancellation|TestVideoConversionQueueRejectsWorkAtCapacity|TestVideoConversionServiceUsesRealSidecarProcess|TestVideoConversionDeduplicatesAndReturnsFreshCache|TestVideoConversionRetriesThenFails|TestVideoConversionCancelsRunningJobE2E'
```

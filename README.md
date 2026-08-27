# dam

`dam` is a startup gate for Unix pipelines. It starts a timer when the first
non-empty read from standard input completes, holds the beginning of the stream
for the requested duration, and then forwards the stream unchanged. The gate
can also be opened by `SIGUSR1`, `SIGUSR2`, or the presence of a regular
file. It also reports its version to stdout without reading standard input.

```text
producer | dam 3s | consumer
```

## Build

Go 1.22 or later is required.

```bash
go build -o dam ./cmd/dam
```

## Usage

```text
dam DURATION
dam [DURATION] --release-on signal:USR1
dam [DURATION] --release-on signal:SIGUSR1
dam [DURATION] --release-on signal:USR2
dam [DURATION] --release-on signal:SIGUSR2
dam [DURATION] --release-on file:PATH
dam -h
dam --help
dam --version
```

`DURATION` uses Go's `time.ParseDuration` syntax, for example `500ms`, `3s`,
`2m`, or `1h30m`.

```bash
printf 'hello' | ./dam 3s
```

`0s` is valid and forwards input immediately. When present, malformed and
negative durations are errors. A release condition may be used without a
duration; at least one duration or release condition is required. At most one
duration is allowed. The duration and each `--release-on TYPE:SOURCE` option
may appear in either order. The release-on option may be repeated and accepts
both separated (`--release-on signal:USR1`) and equals
(`--release-on=signal:USR1`) forms. `signal:USR1` and `signal:SIGUSR1` are
equivalent, as are `signal:USR2` and `signal:SIGUSR2`; other types, signal
names, and casing are invalid. A `file:PATH` condition opens the gate when
`PATH` names a regular file. The first colon separates the condition type from
the path, so additional colons in `PATH`, including a Windows drive-letter
colon, are preserved. The path must not be empty. Relative paths are resolved
from the process's startup working directory; shell-style `~` and environment
variable expansion and path normalization are not performed.

`--version` is valid only as the sole argument and prints `dam <version>\n` to
stdout. Development builds use `dev`; release builds replace the version at
link time.

`-h` and `--help` are equivalent. An exact help argument takes precedence over
all other arguments, prints the help text to stdout, and exits successfully
without reading stdin or starting release monitoring. `--version` also writes
its informational output to stdout without reading stdin. Forms such as
`--help=x`, `--release-on=--help`, and `-help` remain ordinary argument errors.
Argument errors do not print the full help text automatically.

## Stream behavior

- The delay starts on the first non-empty read from stdin, not when the process
  starts. With no input, no timer is started.
- Each configured signal is monitored after argument validation and before the
  first input read. Any configured signal received before input opens the gate
  for all later input.
- File conditions are supported on all targets. Before reading stdin, `dam`
  checks every configured path once. A path that does not exist, including a
  dangling symlink, remains pending. Symlinks are followed; a path releases the
  gate only when its target is a regular file. A directory, FIFO, device,
  symlink loop, permission failure, or other file-status error is fatal while
  the gate is closed.
- Duration, signal, and file conditions are combined with OR: the first
  satisfied condition opens the gate. No stream data is written to stdout
  before a release occurs.
- All initial file checks finish before the first open decision. A fatal result
  from any initial check takes precedence over an existing regular file, `0s`,
  or a pending signal. While the gate remains closed, a fatal file result that
  has already been reported to the release coordinator also takes precedence
  over release events pending at the same decision point.
- EOF does not open the gate early. Data received before EOF is still held until
  a duration, signal, or file release. After the initial file checks succeed,
  empty stdin exits successfully without waiting for a configured release
  condition.
- Input is preserved byte-for-byte, including binary data.
- Before release, stream data is held in a bounded internal buffer. Once that
  buffer is full, ordinary pipe backpressure limits the producer.
- After release, the gate stays open and subsequent input is passed through
  without another delay.
- File monitoring stops when the gate opens or empty stdin reaches EOF. An
  in-progress check is not awaited and its later result is ignored. The polling
  interval and exact detection latency are implementation details, not a
  stable interface.
- Once configured, `SIGUSR1` and/or `SIGUSR2` remain intercepted and ignored
  after release until the process exits, including when the duration or `0s`
  or a file condition opens the gate first. Unconfigured signals are not
  intercepted.
- stdout contains stream data during normal operation. Help and version
  information are written to stdout; other usage messages and I/O errors are
  written to stderr and cause a non-zero exit status.

## Current scope

The current implementation provides a duration-, `SIGUSR1`/`SIGUSR2`-, and
file-based one-way gate plus help and version reporting. Repeated gate
transitions, configurable polling or buffer sizes, spill-to-disk behavior, and
an initially-open mode are not implemented. File conditions work on Windows
and other targets. Signal release is not available on unsupported targets;
those builds accept duration and file-only configurations, but reject any
configuration containing a signal condition.

## Development

```bash
gofmt -w cmd/dam/*.go
go test ./...
go test -race ./...
go vet ./...
```

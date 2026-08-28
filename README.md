# dam

`dam` is a startup gate for Unix pipelines. It starts a timer when the first
non-empty read from standard input completes, holds the beginning of the stream
for the requested duration, and then forwards the stream unchanged. The gate
can also be opened by an absolute local datetime, `SIGUSR1`, `SIGUSR2`, or the
presence of a regular file. It also reports its version to stdout without
reading standard input.

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
dam DEADLINE [--buffer-size SIZE]
dam [DEADLINE] --release-on TYPE:SOURCE [--buffer-size SIZE]
dam -h
dam --help
dam --version
```

`DEADLINE` accepts either Go's `time.ParseDuration` syntax (for example
`500ms`, `3s`, `2m`, or `1h30m`) or an absolute local datetime in exactly
`YYYY-MM-DDTHH:MM` or `YYYY-MM-DDTHH:MM:SS` form. The latter is resolved using
the local timezone at argument validation time. Seconds default to `00` when
omitted. Years must be between `0001` and `9999`; invalid calendar dates,
timezone suffixes, fractional seconds, and non-existent local times during a
daylight-saving transition are rejected. If a local time occurs twice, the
earlier instant is selected.

`--buffer-size SIZE` and `--buffer-size=SIZE` set the maximum amount of
pre-release stream data held in memory. The default is `64K`. `SIZE` must be a
positive integer number of bytes, or a positive integer followed by `K`, `k`,
`M`, `m`, `G`, or `g`; suffixes use binary multipliers (1024, 1024², and
1024³). Values such as `0`, negative numbers, decimals, `KB`, and `KiB` are
invalid. This option does not itself provide a release condition.

```bash
printf 'hello' | ./dam 3s
```

`0s` is valid and forwards input immediately. When present, malformed and
negative durations are errors. A release condition may be used without a
deadline; at least one deadline or release condition is required. At most one
deadline is allowed. A relative duration starts after the first non-empty
read, while an absolute deadline is active from startup and does not wait for
stdin. The deadline and each `--release-on TYPE:SOURCE` option
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

- A relative duration starts on the first non-empty read from stdin, not when
  the process starts. An absolute local deadline is monitored before the
  first read. With no input, a relative timer is never started and stdin exits
  normally without waiting for an absolute deadline.
- Each configured signal is monitored after argument validation and before the
  first input read. Any configured signal received before input opens the gate
  for all later input.
- File conditions are supported on all targets. Before reading stdin, `dam`
  checks every configured path once. A path that does not exist, including a
  dangling symlink, remains pending. Symlinks are followed; a path releases the
  gate only when its target is a regular file. A directory, FIFO, device,
  symlink loop, permission failure, or other file-status error is fatal while
  the gate is closed.
- Relative duration, absolute deadline, signal, and file conditions are
  combined with OR: the first satisfied condition opens the gate. No stream
  data is written to stdout before a release occurs.
- All initial file checks finish before the first open decision. A fatal result
  from any initial check takes precedence over an existing regular file, `0s`,
  a current or past absolute deadline, or a pending signal. While the gate
  remains closed, a fatal file result that has already been reported to the
  release coordinator also takes precedence over release events pending at the
  same decision point.
- EOF does not open the gate early. Data received before EOF is still held until
  a duration, absolute deadline, signal, or file release. After the initial
  file checks succeed, empty stdin exits successfully without waiting for a
  configured release condition.
- Input is preserved byte-for-byte, including binary data.
- Before release, stream data is held in a bounded internal buffer. Its maximum
  size is controlled by `--buffer-size` (64K by default). The buffer grows as
  needed up to that maximum; once full, ordinary pipe backpressure limits the
  producer.
- After release, the gate stays open and subsequent input is passed through
  without another delay.
- Absolute-deadline and file monitoring stop when the gate opens or empty stdin
  reaches EOF. An in-progress file check is not awaited and its later result is
  ignored. The polling interval and exact detection latency are implementation
  details, not a stable interface.
- Once configured, `SIGUSR1` and/or `SIGUSR2` remain intercepted and ignored
  after release until the process exits, including when the duration, `0s`, an
  absolute deadline, or a file condition opens the gate first. Unconfigured
  signals are not intercepted.
- stdout contains stream data during normal operation. Help and version
  information are written to stdout; other usage messages and I/O errors are
  written to stderr and cause a non-zero exit status.

## Current scope

The current implementation provides a duration-, absolute-deadline-,
`SIGUSR1`/`SIGUSR2`-, and file-based one-way gate plus help and version
reporting. Repeated gate
transitions, configurable polling, spill-to-disk behavior, and
an initially-open mode are not implemented. File conditions work on Windows
and other targets. Signal release is not available on unsupported targets;
those builds accept duration, absolute-deadline, and file-only configurations,
including their supported combinations, but reject any configuration containing
a signal condition.

## Development

```bash
gofmt -w cmd/dam/*.go
go test ./...
go test -race ./...
go vet ./...
```

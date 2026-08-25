# dam

`dam` is a startup gate for Unix pipelines. It starts a timer when the first
byte arrives on standard input, holds the beginning of the stream for the
requested duration, and then forwards the stream unchanged. The gate can also
be opened by `SIGUSR1`. It also reports its version without reading standard
input.

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
equivalent; other types, signal names, and casing are invalid.

`--version` is valid only as the sole argument and prints `dam <version>\n`.
Development builds use `dev`; release builds replace the version at link time.

## Stream behavior

- The delay starts on the first non-empty read from stdin, not when the process
  starts. With no input, no timer is started.
- A configured `SIGUSR1` monitor starts after argument validation and before the
  first input read. A signal received before input opens the gate for all later
  input.
- No stream data is written to stdout before the configured duration or signal
  release occurs.
- EOF does not open the gate early. Data received before EOF is still held until
  the requested duration or signal release. Empty stdin exits successfully
  without waiting for a configured signal.
- Input is preserved byte-for-byte, including binary data.
- Before release, stream data is held in a bounded internal buffer. Once that
  buffer is full, ordinary pipe backpressure limits the producer.
- After release, the gate stays open and subsequent input is passed through
  without another delay.
- Once configured, `SIGUSR1` remains intercepted and ignored after release until
  the process exits, including when the duration or `0s` opens the gate first.
- stdout contains stream data only. Usage messages and I/O errors are written to
  stderr and cause a non-zero exit status.

## Current scope

The current implementation provides a duration- and `SIGUSR1`-based, one-way
gate and version reporting. Only `SIGUSR1` is supported as an external release
event. Repeated gate transitions, configurable buffer sizes, spill-to-disk
behavior, and an initially-open mode are not implemented. Signal release is
not available on Windows or other unsupported targets; those builds reject the
signal option explicitly while retaining duration and version support.

## Development

```bash
gofmt -w cmd/dam/*.go
go test ./...
go test -race ./...
go vet ./...
```

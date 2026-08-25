# dam

`dam` is a startup gate for Unix pipelines. It starts a timer when the first
byte arrives on standard input, holds the beginning of the stream for the
requested duration, and then forwards the stream unchanged.

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
```

`DURATION` uses Go's `time.ParseDuration` syntax, for example `500ms`, `3s`,
`2m`, or `1h30m`.

```bash
printf 'hello' | ./dam 3s
```

`0s` is valid and forwards input immediately. Missing, extra, malformed, and
negative durations are errors.

## Stream behavior

- The delay starts on the first non-empty read from stdin, not when the process
  starts. With no input, no timer is started.
- No stream data is written to stdout before the delay expires.
- EOF does not open the gate early. Data received before EOF is still held until
  the requested release time.
- Input is preserved byte-for-byte, including binary data.
- Before release, stream data is held in a bounded in-memory buffer. Once the
  implementation cannot accept more data, ordinary pipe backpressure limits
  the producer.
- After release, the gate stays open and subsequent input is passed through
  without another delay.
- stdout contains stream data only. Usage messages and I/O errors are written to
  stderr and cause a non-zero exit status.

## Current scope

The current implementation provides one duration-based, one-way gate. External
release events, repeated gate transitions, configurable buffer sizes,
spill-to-disk behavior, and an initially-open mode are not implemented.

## Development

```bash
gofmt -w cmd/dam/*.go
go test ./...
go test -race ./...
go vet ./...
```

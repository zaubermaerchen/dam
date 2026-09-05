# dam

`dam` is a release gate for Unix pipelines. With a relative duration, it starts
a timer when the first non-empty read from standard input completes and holds
the beginning of the stream until that duration elapses. The gate can instead
be opened by an absolute local datetime, `SIGUSR1`, `SIGUSR2`, or the presence
of a regular file. It is a one-way gate: once released, it stays open and
forwards the rest of the stream unchanged. It also reports its version to
stdout without reading standard input.

```text
producer | dam duration:3s | consumer
```

## Choosing a tool

Several Unix tools control when commands run or data becomes available, but
they serve different purposes:

| Tool | Primary purpose | Producer can start immediately? | What determines downstream availability? |
| --- | --- | --- | --- |
| `at` | Schedule command execution | No, for the scheduled command | Scheduled time |
| [`delay`](https://github.com/rom1v/delay) | Apply a constant delay to stream data | Yes | Per-data delay |
| `sponge` | Read all input before writing output | Yes | EOF |
| `dam` | Gate a running pipeline | Yes | Configured release condition |

For example:

```bash
slow-producer | dam datetime:2099-12-31T23:59 | consumer
```

Here, `slow-producer` starts immediately and can fill `dam`'s bounded buffer,
with normal pipe backpressure after that. `consumer` receives no data until the
gate opens at the configured local deadline. Use a command scheduler such as
`at` instead when the producer itself should not start until the scheduled
time.

## Build

Go 1.22 or later is required.

```bash
go build -o dam ./cmd/dam
```

## Usage

```text
dam CONDITION [--or CONDITION]... [--buffer-size SIZE]
dam -h
dam --help
dam --version
```

Every release condition is a prefixed positional `CONDITION`:

```text
duration:DURATION
datetime:YYYY-MM-DDTHH:MM[:SS]
signal:USR1
signal:SIGUSR1
signal:USR2
signal:SIGUSR2
file:PATH
```

`duration:DURATION` accepts Go's `time.ParseDuration` syntax (for example
`500ms`, `3s`, `2m`, or `1h30m`). `0s` is valid and is immediately satisfied;
negative and malformed durations are errors. Positive duration conditions
start from the same first non-empty stdin read, and multiple distinct durations
are allowed.

`datetime:YYYY-MM-DDTHH:MM[:SS]` is an absolute local datetime monitored from
startup. Seconds default to `00` when omitted. Years must be between `0001` and
`9999`; invalid calendar dates, timezone suffixes, fractional seconds, and
non-existent local times during a daylight-saving transition are rejected. If a
local time occurs twice, the earlier instant is selected. Multiple distinct
datetime conditions are allowed.

`signal:USR1` and `signal:SIGUSR1` are equivalent, as are `signal:USR2` and
`signal:SIGUSR2`. Signal conditions are available on supported Unix targets.
`file:PATH` releases when `PATH` resolves to a regular file on every target.
The first colon separates the type from the source, so additional colons in a
file path, including a Windows drive-letter colon, are preserved. Paths are
not normalized or expanded; relative paths use the startup working directory.

`--buffer-size SIZE` and `--buffer-size=SIZE` set the maximum amount of
pre-release stream data held in memory. The default is `64K`. `SIZE` must be a
positive integer number of bytes, or a positive integer followed by `K`, `k`,
`M`, `m`, `G`, or `g`; suffixes use binary multipliers (1024, 1024², and
1024³). Values such as `0`, negative numbers, decimals, `KB`, and `KiB` are
invalid. This option does not itself provide a release condition.

```bash
printf 'hello' | ./dam duration:3s
```

At least one condition is required. Conditions and `--buffer-size` may appear
in any order. Use `--or CONDITION` between alternatives; the equals form
`--or=CONDITION` is also accepted. Multiple alternatives are combined with OR.
Conditions joined by the exact literal ` && ` inside one argument form an AND
group: every member must be satisfied. Quote an AND group so the shell passes
the literal ` && ` to `dam`:

```bash
dam 'signal:USR1 && file:/tmp/ready'
```

In PowerShell, single quotes provide the same protection. In `cmd.exe`, use
double quotes around a compound value, for example
`dam "file:C:\ready && file:C:\approved"`; shell quoting only affects
how the argument reaches `dam`, not the condition parser itself.

Each AND member is latched once satisfied, so satisfaction order does not
matter and a file may be removed after its latch without closing the gate.
Equivalent duration values share one logical latch based on their parsed value
(for example, `duration:60s` and `duration:1m`), and equivalent datetime values
share one latch based on their resolved instant. Distinct values remain
independent conditions. A file path must not be empty; shell-style `~` and
environment-variable expansion are not performed.

`--version` is valid only as the sole argument and prints `dam <version>\n` to
stdout. Development builds use `dev`; release builds replace the version at
link time. The release workflow verifies the injected version with the Linux
amd64 artifact before publishing release archives.

`-h` and `--help` are equivalent. An exact help argument takes precedence over
all other arguments, prints the help text to stdout, and exits successfully
without reading stdin or starting release monitoring. `--version` also writes
its informational output to stdout without reading stdin. Forms such as
`--help=x`, `-help`, and other non-exact spellings remain ordinary argument
errors.
Argument errors do not print the full help text automatically.

## Migrating from v0.3.x

Release note: v0.4.0 is a breaking release for the command-line grammar. Bare
deadlines and the `--release-on` option are removed; prefix each condition and join
alternatives with `--or` instead. Existing invocations migrate as follows:

```text
dam 30s
  -> dam duration:30s

dam 2026-09-03T18:00
  -> dam datetime:2026-09-03T18:00

dam --release-on signal:USR1
  -> dam signal:USR1

dam --release-on=signal:USR1
  -> dam signal:USR1

dam --release-on signal:USR1 --release-on file:/tmp/ready
  -> dam signal:USR1 --or file:/tmp/ready

dam --release-on "signal:USR1 && file:/tmp/ready" --release-on signal:USR2
  -> dam "signal:USR1 && file:/tmp/ready" --or signal:USR2
```

The old forms above are migration references only and are rejected by v0.4.0.
See the usage and stream behavior sections for the complete condition and
monitoring contract.

## Stream behavior

- Every duration condition starts on the first non-empty read from stdin, not
  when the process starts. Every datetime condition is monitored from startup.
  With no input, a relative timer is never started and `dam` exits normally
  without waiting for a datetime condition.
- Each configured signal is monitored after argument validation and before the
  first input read. Any configured signal received before input opens the gate
  for all later input.
- File conditions are supported on all targets. Before reading stdin, `dam`
  checks every configured path once. A path that does not exist, including a
  dangling symlink, remains pending. Symlinks are followed; a path releases the
  gate only when its target is a regular file. A directory, FIFO, device,
  symlink loop, permission failure, or other file-status error is fatal while
  the gate is closed.
- The positional conditions and each `--or` group are combined with OR: the
  first satisfied release group opens the gate. Conditions within one argument
  joined by ` && ` are ANDed. No stream data is written to stdout before a
  release occurs.
- All initial file checks finish before the first open decision. A fatal result
  from any initial check takes precedence over an existing regular file, `0s`,
  a current or past datetime, or a pending signal. While the gate
  remains closed, a fatal file result that has already been reported to the
  release coordinator also takes precedence over release events pending at the
  same decision point.
- Startup validates all members before normal stream processing: syntax first,
  then platform capabilities, then every initial file probe. The initial file
  probe barrier covers every OR group; an initial fatal is reported in
  configuration order even if another group is already satisfiable. A signal
  received while that barrier is running is latched for its group but cannot
  override an initial fatal.
- EOF does not open the gate early. Data received before EOF is still held until
  a duration, datetime, signal, or file release. After the initial
  file checks succeed, empty stdin exits successfully without waiting for a
  configured release condition.
- Input is preserved byte-for-byte, including binary data.
- Before release, stream data is held in a bounded internal buffer. Its maximum
  size is controlled by `--buffer-size` (64K by default). The buffer grows as
  needed up to that maximum; once full, ordinary pipe backpressure limits the
  producer.
- After release, the gate stays open and subsequent input is passed through
  without another delay.
- Duration, datetime, and file monitoring stop when the gate opens or empty
  stdin reaches EOF. An in-progress file check is not awaited and its later
  result is ignored. The polling interval and exact detection latency are
  implementation details, not a stable interface.
- Once configured, `SIGUSR1` and/or `SIGUSR2` remain intercepted and ignored
  after release until the process exits, including when a duration, `0s`, a
  datetime, or a file condition opens the gate first. Unconfigured
  signals are not intercepted.
- stdout contains stream data during normal operation. Help and version
  information are written to stdout; other usage messages and I/O errors are
  written to stderr and cause a non-zero exit status.

## Current scope

The current implementation provides a duration-, datetime-,
`SIGUSR1`/`SIGUSR2`-, and file-based one-way gate plus help and version
reporting. Repeated gate
transitions, configurable polling, spill-to-disk behavior, and
an initially-open mode are not implemented. File conditions work on Windows
and other targets. Signal release is not available on unsupported targets;
those builds accept duration, datetime, and file-only configurations,
including their supported combinations, but reject any configuration containing
a signal condition.

## Development

```bash
gofmt -w cmd/dam/*.go
go test ./...
go test -race ./...
go vet ./...
```

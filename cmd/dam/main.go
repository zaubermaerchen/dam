package main

// This file implements the dam command's delayed stdin-to-stdout forwarding.

import (
	"fmt"
	"io"
	"os"
	"time"
)

const preReleaseBufferSize = 64 * 1024

func main() {
	os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

func run(args []string, input io.Reader, output, diagnostics io.Writer) int {
	delay, err := parseDelay(args)
	if err != nil {
		writeDiagnostic(diagnostics, err)
		return 1
	}

	if err := forward(input, output, delay); err != nil {
		writeDiagnostic(diagnostics, err)
		return 1
	}
	return 0
}

func parseDelay(args []string) (time.Duration, error) {
	if len(args) != 1 {
		return 0, fmt.Errorf("usage: dam DURATION")
	}

	delay, err := time.ParseDuration(args[0])
	if err != nil {
		return 0, fmt.Errorf("invalid duration %q: %w", args[0], err)
	}
	if delay < 0 {
		return 0, fmt.Errorf("duration must not be negative")
	}
	return delay, nil
}

func forward(input io.Reader, output io.Writer, delay time.Duration) error {
	if delay == 0 {
		_, err := io.Copy(output, input)
		return err
	}

	held := make([]byte, preReleaseBufferSize)
	firstN, firstErr := readFirst(input, held)
	if firstN == 0 {
		if firstErr == io.EOF {
			return nil
		}
		return firstErr
	}

	// The first non-empty read, rather than process startup, starts the delay.
	timer := time.NewTimer(delay)
	defer timer.Stop()

	// A read that returns data and an error still starts the delay. There is no
	// need to start another read when the reader has already ended.
	if firstErr != nil {
		<-timer.C
		if err := writeAll(output, held[:firstN]); err != nil {
			return err
		}
		if firstErr == io.EOF {
			return nil
		}
		return firstErr
	}

	return forwardDelayed(input, output, timer, held, firstN)
}

type readResult struct {
	n   int
	err error
}

func readFirst(input io.Reader, buffer []byte) (int, error) {
	for {
		n, err := input.Read(buffer)
		if n < 0 || n > len(buffer) {
			return 0, fmt.Errorf("invalid input read count %d", n)
		}
		if n > 0 {
			return n, err
		}
		if err != nil {
			return 0, err
		}
	}
}

func forwardDelayed(input io.Reader, output io.Writer, timer *time.Timer, held []byte, heldN int) error {
	timerC := timer.C
	for heldN < len(held) {
		readBuffer := held[heldN:]
		readDone := make(chan readResult, 1)
		go readInto(input, readBuffer, readDone)

		select {
		case <-timerC:
			timerC = nil
			if err := writeAll(output, held[:heldN]); err != nil {
				return err
			}
			return forwardReadResult(input, output, readBuffer, <-readDone)
		case result := <-readDone:
			heldN += result.n
			if result.err != nil {
				if timerC != nil {
					<-timerC
					timerC = nil
				}
				if err := writeAll(output, held[:heldN]); err != nil {
					return err
				}
				if result.err == io.EOF {
					return nil
				}
				return result.err
			}

			// If the timer became ready with the read result, release without
			// starting another read. Otherwise, the next iteration uses the
			// next unused tail of the fixed buffer.
			select {
			case <-timerC:
				timerC = nil
				if err := writeAll(output, held[:heldN]); err != nil {
					return err
				}
				_, err := io.Copy(output, input)
				return err
			default:
			}
		}
	}

	if timerC != nil {
		<-timerC
	}
	if err := writeAll(output, held); err != nil {
		return err
	}
	_, err := io.Copy(output, input)
	return err
}

func readInto(input io.Reader, buffer []byte, readDone chan<- readResult) {
	n, err := input.Read(buffer)
	if n < 0 || n > len(buffer) {
		readDone <- readResult{err: fmt.Errorf("invalid input read count %d", n)}
		return
	}
	readDone <- readResult{n: n, err: err}
}

func forwardReadResult(input io.Reader, output io.Writer, readBuffer []byte, result readResult) error {
	if result.n > 0 {
		if err := writeAll(output, readBuffer[:result.n]); err != nil {
			return err
		}
	}
	if result.err != nil {
		if result.err == io.EOF {
			return nil
		}
		return result.err
	}
	_, err := io.Copy(output, input)
	return err
}

func writeAll(output io.Writer, data []byte) error {
	for len(data) > 0 {
		n, err := output.Write(data)
		if n < 0 || n > len(data) {
			return fmt.Errorf("invalid output write count %d", n)
		}
		data = data[n:]
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
	}
	return nil
}

func writeDiagnostic(diagnostics io.Writer, err error) {
	_, _ = fmt.Fprintln(diagnostics, err)
}

//go:build !windows

package main

import (
	"context"
	"errors"
)

// decodeAudioOS: only Windows has decoders Jasqet can use directly; elsewhere ffmpeg does it.
func decodeAudioOS(ctx context.Context, in, out string, progress func(float64)) error {
	return errors.New("no built-in decoder: ffmpeg is needed for this file")
}

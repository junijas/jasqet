//go:build !windows && !darwin

package main

import "context"

func defenderScan(ctx context.Context, p string) (string, bool) { return "", false }

func markFromInternet(p string, d *Download) {}

func keepInternetTag(src, dst string) {}

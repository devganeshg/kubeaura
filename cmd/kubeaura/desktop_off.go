//go:build !desktop

package main

// runDesktop is a stub for binaries built without the desktop tag (the
// default CGO-free build). It reports that no native window is available so
// main can explain how to get one.
func runDesktop(string) bool { return false }

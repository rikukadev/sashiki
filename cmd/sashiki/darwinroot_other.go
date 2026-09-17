//go:build !darwin

package main

// prepareDarwinRoot は darwin 以外では何もしない(init --platform darwin は darwin 専用)。
func prepareDarwinRoot(root string) error { return nil }

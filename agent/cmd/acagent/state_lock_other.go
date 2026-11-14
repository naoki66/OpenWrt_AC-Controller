//go:build !linux

package main

func lockAgentState(_ string) (func(), error) { return func() {}, nil }

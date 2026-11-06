//go:build !linux && !darwin

package main

import "errors"

var errNoUnit = errors.New("held sessions and the rota daemon run on linux and macOS")

func unitFile(string) (string, error)                   { return "", errNoUnit }
func writeUnit(_, _ string, _ []string) (string, error) { return "", errNoUnit }
func removeUnit(string) error                           { return errNoUnit }

// Package nofigure names one way a usage reading fails, so the store can
// tell it from the rest without the SDK growing a name for it: a reply in
// which the usage endpoint reported windows and gave none of them a figure.
//
// lib wraps Err in the error its claude reading returns; the store backs off
// on it, as it does on a 429, so a reply that keeps coming back that way is
// not asked for again on every look.
package nofigure

import "errors"

// Err is a usage reply whose windows all came without a figure.
var Err = errors.New("the usage endpoint gave no window a figure")

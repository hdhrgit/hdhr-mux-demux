//go:build !unix

package main

import "errors"

func execSelf() error { return errors.New("restarting in place is not supported here") }

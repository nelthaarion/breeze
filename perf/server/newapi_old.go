//go:build !newapi

package main

import breeze "github.com/nelthaarion/breeze/v2"

func applyNewAPI(app *breeze.Breeze)       {}
func newMW(name string) breeze.HandlerFunc { return nil }

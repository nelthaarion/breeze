package main

import (
	"fmt"

	"github.com/nelthaarion/breeze/v2/events"
)

func RegisterListeners() {

	events.On(
		UserRegistered{},
		func(
			ctx *events.Context,
			event UserRegistered,
		) error {

			fmt.Println(
				"📧 Sending welcome email:",
				event.Email,
			)

			return nil
		},
	)

	events.On(
		UserRegistered{},
		func(
			ctx *events.Context,
			event UserRegistered,
		) error {

			fmt.Println(
				"📝 Audit:",
				event.ID,
			)

			return nil
		},
	)
}

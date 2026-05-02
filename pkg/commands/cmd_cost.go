package commands

import (
	"context"
)

func costCommand() Definition {
	return Definition{
		Name:        "cost",
		Description: "Show current session cost breakdown and cache statistics",
		Usage:       "/cost",
		Handler: func(_ context.Context, req Request, rt *Runtime) error {
			if rt == nil || rt.GetCostBreakdown == nil {
				return req.Reply(unavailableMsg)
			}
			breakdown := rt.GetCostBreakdown()
			if breakdown == "" {
				return nil
			}
			return req.Reply(breakdown)
		},
	}
}

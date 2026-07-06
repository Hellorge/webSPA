package main

import (
	"encoding/json"

	"gogogo/modules/actions"
	"gogogo/modules/metrics"
)

// app-level dynamic-action handlers — generic gogogo endpoints not owned
// by any module.

func actionMetrics(c *actions.RouteCtx) {
	writeJSON(c, metrics.Get().GetSnapshot())
}

func writeJSON(c *actions.RouteCtx, v interface{}) {
	body, err := json.Marshal(v)
	if err != nil {
		c.Resp.ServerError()
		return
	}
	c.Resp.HeadID = c.App.HeadJSON()
	c.Resp.Body = body
}

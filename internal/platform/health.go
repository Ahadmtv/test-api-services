package platform

import (
	"context"
	"time"

	"github.com/gofiber/fiber/v3"
)

const readyTimeout = 2 * time.Second

// Health exposes liveness and readiness probes.
type Health struct {
	DB Pinger
}

// Live reports that the process is alive. It never depends on the database.
func (h Health) Live(c fiber.Ctx) error {
	return c.Status(200).Type("application/json").JSON(map[string]string{
		"status": "alive",
	})
}

// Ready reports that the service can serve traffic, which requires a reachable
// database.
func (h Health) Ready(c fiber.Ctx) error {
	ctx, cancel := context.WithTimeout(c.Context(), readyTimeout)
	defer cancel()

	if err := h.DB.Ping(ctx); err != nil {
		return DatabaseUnavailable(err)
	}

	return c.Status(200).Type("application/json").JSON(map[string]string{
		"status": "ready",
	})
}

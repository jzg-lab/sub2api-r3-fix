package admin

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

func (h *AccountHandler) ManualQualityTemplates(c *gin.Context) {
	templates, err := h.accountTestService.ManualQualityTemplates(c.Request.Context())
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, gin.H{"templates": templates, "defaults": service.DefaultManualQualityTemplates()})
}

func (h *AccountHandler) SaveManualQualityTemplate(c *gin.Context) {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 32<<10)
	var input service.ManualQualityTemplate
	if err := c.ShouldBindJSON(&input); err != nil {
		response.BadRequest(c, "Invalid template")
		return
	}
	if err := input.Validate(); err != nil {
		response.BadRequest(c, err.Error())
		return
	}
	if err := h.accountTestService.SaveManualQualityTemplate(c.Request.Context(), input); err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, input)
}

func manualQualityAccountID(c *gin.Context) (int64, bool) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		response.BadRequest(c, "Invalid account ID")
		return 0, false
	}
	return id, true
}

func (h *AccountHandler) ManualQualityResult(c *gin.Context) {
	id, ok := manualQualityAccountID(c)
	if !ok {
		return
	}
	if _, err := h.adminService.GetAccount(c.Request.Context(), id); err != nil {
		response.ErrorFrom(c, err)
		return
	}
	store, err := h.accountTestService.ManualQualityStore()
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	result, err := store.GetManualQuality(c.Request.Context(), id, c.Query("model"), c.Query("kind"))
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, result)
}

func (h *AccountHandler) ManualQualitySummaries(c *gin.Context) {
	raw := strings.Split(c.Query("ids"), ",")
	if len(raw) == 0 || len(raw) > 200 {
		response.BadRequest(c, "Provide 1–200 account IDs")
		return
	}
	ids := make([]int64, 0, len(raw))
	for _, part := range raw {
		id, err := strconv.ParseInt(part, 10, 64)
		if err != nil || id <= 0 {
			response.BadRequest(c, "Invalid account ID")
			return
		}
		ids = append(ids, id)
	}
	store, err := h.accountTestService.ManualQualityStore()
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	results, err := store.ListManualQuality(c.Request.Context(), ids)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, results)
}

func (h *AccountHandler) RunManualQuality(c *gin.Context) {
	id, ok := manualQualityAccountID(c)
	if !ok {
		return
	}
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 32<<10)
	var input service.ManualQualityRequest
	if err := c.ShouldBindJSON(&input); err != nil {
		response.BadRequest(c, "Invalid test request")
		return
	}
	if err := input.Validate(); err != nil {
		response.BadRequest(c, err.Error())
		return
	}
	if strings.TrimSpace(input.Model) == "" || len(input.Model) > 200 {
		response.BadRequest(c, "Select a text model (maximum 200 bytes)")
		return
	}
	// Heartbeats keep slow reasoning tests alive through reverse proxies.
	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")
	c.Header("X-Accel-Buffering", "no")
	c.Writer.Flush()
	type completion struct {
		result *service.ManualQualityResult
		err    error
	}
	done := make(chan completion, 1)
	ctx := c.Request.Context()
	go func() {
		defer func() {
			if recover() != nil {
				log.Print("manual quality worker panicked")
				done <- completion{err: errors.New("the test was interrupted; its upstream outcome is unknown")}
			}
		}()
		result, err := h.accountTestService.RunManualQuality(ctx, id, input)
		done <- completion{result, err}
	}()
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if _, err := fmt.Fprint(c.Writer, ": waiting\n\n"); err != nil {
				return
			}
			c.Writer.Flush()
		case outcome := <-done:
			payload := gin.H{"result": outcome.result, "saved": outcome.err == nil}
			if outcome.err != nil {
				if outcome.result != nil {
					payload["error"] = "The test finished but its result could not be saved."
				} else {
					payload["error"] = outcome.err.Error()
				}
			}
			data, _ := json.Marshal(payload)
			_, _ = fmt.Fprintf(c.Writer, "data: %s\n\n", data)
			c.Writer.Flush()
			return
		}
	}
}

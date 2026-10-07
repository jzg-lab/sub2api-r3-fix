package admin

import (
	"encoding/json"
	"net/http"
	"strconv"

	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

func (h *OpenAIOAuthHandler) AccountTOTP(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		response.ErrorFrom(c, service.ErrAccountTOTPInvalid)
		return
	}
	if h.accountTOTP == nil {
		response.ErrorFrom(c, service.ErrAccountTOTPKey)
		return
	}
	var status *service.AccountTOTPStatus
	if c.Request.Method == http.MethodGet {
		status, err = h.accountTOTP.Status(c.Request.Context(), id)
	} else {
		c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 2048)
		var raw map[string]json.RawMessage
		if c.ShouldBindJSON(&raw) != nil {
			response.ErrorFrom(c, service.ErrAccountTOTPInvalid)
			return
		}
		for key, value := range raw {
			switch key {
			case "totp_secret", "mfa_secret", "expected_authorization_revision", "clear":
				if string(value) == "null" {
					response.ErrorFrom(c, service.ErrAccountTOTPInvalid)
					return
				}
			default:
				response.ErrorFrom(c, service.ErrAccountTOTPInvalid)
				return
			}
		}
		encoded, _ := json.Marshal(raw)
		var input service.AccountTOTPInput
		if json.Unmarshal(encoded, &input) != nil {
			response.ErrorFrom(c, service.ErrAccountTOTPInvalid)
			return
		}
		status, err = h.accountTOTP.Save(c.Request.Context(), id, input)
	}
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, status)
}

func (h *OpenAIOAuthHandler) SetAccountTOTP(s *service.AccountTOTPService) { h.accountTOTP = s }

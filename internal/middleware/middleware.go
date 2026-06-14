package middleware

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"strings"
	"time"

	"battery-rental/internal/auth"
	"battery-rental/internal/config"
	"battery-rental/internal/models"
	"battery-rental/internal/redisx"
	"battery-rental/internal/utils"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

func RequestID() gin.HandlerFunc {
	return func(c *gin.Context) {
		rid := c.GetHeader("X-Request-ID")
		if rid == "" {
			rid = uuid.NewString()
		}
		c.Set("request_id", rid)
		c.Header("X-Request-ID", rid)
		c.Next()
	}
}

func CORSMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Writer.Header().Set("Access-Control-Allow-Origin", "*")
		c.Writer.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
		c.Writer.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization, X-Request-ID, X-Idempotent-Key")
		c.Writer.Header().Set("Access-Control-Max-Age", "86400")
		if c.Request.Method == "OPTIONS" {
			c.AbortWithStatus(204)
			return
		}
		c.Next()
	}
}

func Recovery() gin.HandlerFunc {
	return func(c *gin.Context) {
		defer func() {
			if err := recover(); err != nil {
				log.Printf("[PANIC] %v", err)
				utils.Fail(c, 500, "服务器内部错误")
				c.Abort()
			}
		}()
		c.Next()
	}
}

func AuthRequired(roles ...string) gin.HandlerFunc {
	return func(c *gin.Context) {
		authHeader := c.GetHeader("Authorization")
		if authHeader == "" {
			utils.Fail(c, 401, "未提供认证凭证")
			c.Abort()
			return
		}
		parts := strings.Split(authHeader, " ")
		if len(parts) != 2 || parts[0] != "Bearer" {
			utils.Fail(c, 401, "认证凭证格式错误")
			c.Abort()
			return
		}
		claims, err := auth.ParseToken(parts[1])
		if err != nil {
			utils.Fail(c, 401, "认证凭证无效或已过期")
			c.Abort()
			return
		}
		blacklist, _ := redisx.Get(c.Request.Context(), "jwt:blacklist:"+parts[1])
		if blacklist == "1" {
			utils.Fail(c, 401, "认证凭证已失效")
			c.Abort()
			return
		}
		if len(roles) > 0 {
			roleOk := false
			for _, r := range roles {
				if claims.Role == r {
					roleOk = true
					break
				}
			}
			if !roleOk {
				utils.Fail(c, 403, "无权限访问")
				c.Abort()
				return
			}
		}
		c.Set("user_id", claims.UserID)
		c.Set("user_phone", claims.Phone)
		c.Set("user_role", claims.Role)
		c.Next()
	}
}

func Idempotent(ttlSeconds ...int) gin.HandlerFunc {
	ttl := 24 * time.Hour
	if len(ttlSeconds) > 0 && ttlSeconds[0] > 0 {
		ttl = time.Duration(ttlSeconds[0]) * time.Second
	}
	return func(c *gin.Context) {
		key := c.GetHeader("X-Idempotent-Key")
		if key == "" {
			c.Next()
			return
		}
		ctx := c.Request.Context()
		userID, _ := c.Get("user_id")
		cacheKey := "idempotent:" + key
		if userID != nil {
			cacheKey += ":" + utils.ToString(userID)
		}
		existing, err := redisx.Get(ctx, cacheKey)
		if err == nil && existing != "" {
			var resp utils.Response
			_ = json.Unmarshal([]byte(existing), &resp)
			reqID, _ := c.Get("request_id")
			resp.RequestID = utils.ToString(reqID)
			c.JSON(http.StatusOK, resp)
			c.Abort()
			return
		}
		c.Set("idempotent_key", cacheKey)
		c.Set("idempotent_ttl", ttl)
		c.Next()
	}
}

func SaveIdempotentResult() gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Next()
		key, ok := c.Get("idempotent_key")
		if !ok {
			return
		}
		ttl, _ := c.Get("idempotent_ttl")
		ttlDur, ok := ttl.(time.Duration)
		if !ok {
			ttlDur = 24 * time.Hour
		}
		body, exists := c.Get("response_body")
		if !exists {
			return
		}
		bodyStr, ok := body.(string)
		if !ok {
			return
		}
		_ = redisx.SetEX(context.Background(), key.(string), bodyStr, ttlDur)
	}
}

func CaptureResponseBody() gin.HandlerFunc {
	return func(c *gin.Context) {
		blw := &bodyLogWriter{body: make([]byte, 0), ResponseWriter: c.Writer}
		c.Writer = blw
		c.Next()
		c.Set("response_body", string(blw.body))
	}
}

type bodyLogWriter struct {
	gin.ResponseWriter
	body []byte
}

func (w *bodyLogWriter) Write(b []byte) (int, error) {
	w.body = append(w.body, b...)
	return w.ResponseWriter.Write(b)
}

func DeviceAuth() gin.HandlerFunc {
	return func(c *gin.Context) {
		token := c.GetHeader("X-Device-Token")
		if token == "" {
			utils.Fail(c, 401, "缺少设备凭证")
			c.Abort()
			return
		}
		expected := "dev-" + config.AppConfig.JWTSecret[:8]
		if token != expected && !strings.HasPrefix(token, "cab-") {
			utils.Fail(c, 401, "设备凭证无效")
			c.Abort()
			return
		}
		if strings.HasPrefix(token, "cab-") {
			cabNo := strings.TrimPrefix(token, "cab-")
			c.Set("cabinet_no", cabNo)
		}
		c.Next()
	}
}

func GetUserID(c *gin.Context) uint64 {
	v, ok := c.Get("user_id")
	if !ok {
		return 0
	}
	if uid, ok := v.(uint64); ok {
		return uid
	}
	return 0
}

func GetUserRole(c *gin.Context) string {
	v, ok := c.Get("user_role")
	if !ok {
		return ""
	}
	if r, ok := v.(string); ok {
		return r
	}
	return ""
}

func GetCabinetNo(c *gin.Context) string {
	v, ok := c.Get("cabinet_no")
	if !ok {
		return ""
	}
	return v.(string)
}

func IsAdmin(c *gin.Context) bool {
	return GetUserRole(c) == string(models.RoleAdmin) || GetUserRole(c) == string(models.RoleOperator)
}

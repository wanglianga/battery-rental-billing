package utils

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"math"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

type Response struct {
	Code    int         `json:"code"`
	Message string      `json:"message"`
	Data    interface{} `json:"data,omitempty"`
	RequestID string    `json:"request_id,omitempty"`
}

func OK(c *gin.Context, data interface{}) {
	reqID, _ := c.Get("request_id")
	c.JSON(200, Response{
		Code:      0,
		Message:   "ok",
		Data:      data,
		RequestID: toString(reqID),
	})
}

func Fail(c *gin.Context, code int, message string) {
	reqID, _ := c.Get("request_id")
	c.JSON(200, Response{
		Code:      code,
		Message:   message,
		RequestID: toString(reqID),
	})
}

func FailWithData(c *gin.Context, code int, message string, data interface{}) {
	reqID, _ := c.Get("request_id")
	c.JSON(200, Response{
		Code:      code,
		Message:   message,
		Data:      data,
		RequestID: toString(reqID),
	})
}

func ToString(v interface{}) string {
	if v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	return fmt.Sprintf("%v", v)
}

func toString(v interface{}) string {
	return ToString(v)
}

func GenOrderNo() string {
	now := time.Now()
	return fmt.Sprintf("BR%s%06d%s",
		now.Format("20060102150405"),
		now.Nanosecond()/1000%1000000,
		RandHex(4),
	)
}

func GenPayNo() string {
	now := time.Now()
	return fmt.Sprintf("PAY%s%s", now.Format("20060102150405"), RandHex(6))
}

func GenTxnID() string {
	return uuid.NewString()
}

func GenReportNo(prefix string, cabinetNo string) string {
	now := time.Now()
	return fmt.Sprintf("RPT%s%s%s%s",
		prefix,
		cabinetNo,
		now.Format("20060102150405"),
		RandHex(4),
	)
}

func RandHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func ParseUint(s string) uint64 {
	v, _ := strconv.ParseUint(s, 10, 64)
	return v
}

func ParseInt(s string) int {
	v, _ := strconv.Atoi(s)
	return v
}

type DefaultTime time.Time

func OrDefaultTime(t *time.Time, fallback string) DefaultTime {
	if t != nil {
		return DefaultTime(*t)
	}
	dt, _ := time.Parse("2006-01-02 15:04", fallback)
	return DefaultTime(dt)
}

func (d DefaultTime) Format(layout string) string {
	return time.Time(d).Format(layout)
}

func CalcFee(durationSec int64, freeMin, firstMin int, firstPrice int64, unitMin int, unitPrice int64, dailyCap int64, maxDays int, maxFee int64) (int64, bool, int) {
	totalMin := int(math.Ceil(float64(durationSec) / 60.0))
	if totalMin <= freeMin {
		return 0, false, 0
	}
	remainMin := totalMin - freeMin
	days := 0
	remainAfter := remainMin
	if maxDays > 0 && dailyCap > 0 {
		days = remainMin / (24 * 60)
		if days > maxDays {
			days = maxDays
		}
		remainAfter = remainMin - days*24*60
		if remainAfter < 0 {
			remainAfter = 0
		}
	}
	var periodFee int64
	if remainAfter > 0 {
		if remainAfter <= firstMin {
			periodFee = firstPrice
		} else {
			periodFee = firstPrice
			afterFirst := remainAfter - firstMin
			units := (afterFirst + unitMin - 1) / unitMin
			periodFee += int64(units) * unitPrice
		}
	}
	dailyFee := int64(days) * dailyCap
	total := dailyFee + periodFee
	capHit := false
	if maxFee > 0 && total > maxFee {
		total = maxFee
		capHit = true
	}
	if dailyCap > 0 && periodFee > dailyCap && days == 0 {
		periodFee = dailyCap
		total = dailyFee + periodFee
		capHit = true
	}
	return total, capHit, days
}

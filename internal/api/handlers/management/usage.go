package management

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/redisqueue"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/usagestore"
)

type usageQueueRecord []byte

func (r usageQueueRecord) MarshalJSON() ([]byte, error) {
	if json.Valid(r) {
		return append([]byte(nil), r...), nil
	}
	return json.Marshal(string(r))
}

// GetUsageQueue pops queued usage records from the usage queue.
func (h *Handler) GetUsageQueue(c *gin.Context) {
	if h == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "handler unavailable"})
		return
	}

	count, errCount := parseUsageQueueCount(c.Query("count"))
	if errCount != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": errCount.Error()})
		return
	}

	items := redisqueue.PopOldest(count)
	records := make([]usageQueueRecord, 0, len(items))
	for _, item := range items {
		records = append(records, usageQueueRecord(append([]byte(nil), item...)))
	}

	c.JSON(http.StatusOK, records)
}

// GetUsageSummary returns persisted usage statistics and estimated billing.
func (h *Handler) GetUsageSummary(c *gin.Context) {
	if h == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "handler unavailable"})
		return
	}

	since, label, errSince := parseUsageSince(c.Query("since"))
	if errSince != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": errSince.Error()})
		return
	}
	recentLimit, errLimit := parseUsageRecentLimit(c.Query("recent"))
	if errLimit != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": errLimit.Error()})
		return
	}

	summary, errSummary := usagestore.GetSummary(usagestore.Query{
		Since:       since,
		SinceLabel:  label,
		RecentLimit: recentLimit,
	})
	if errSummary != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": errSummary.Error()})
		return
	}
	c.JSON(http.StatusOK, summary)
}

// DeleteUsageSummary clears persisted usage statistics.
func (h *Handler) DeleteUsageSummary(c *gin.Context) {
	if h == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "handler unavailable"})
		return
	}
	if err := usagestore.Reset(); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

func parseUsageQueueCount(value string) (int, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return 1, nil
	}
	count, errCount := strconv.Atoi(value)
	if errCount != nil || count <= 0 {
		return 0, errors.New("count must be a positive integer")
	}
	return count, nil
}

func parseUsageSince(value string) (time.Time, string, error) {
	value = strings.TrimSpace(strings.ToLower(value))
	if value == "" {
		value = "24h"
	}
	if value == "all" {
		return time.Time{}, value, nil
	}
	duration, err := parseUsageDuration(value)
	if err != nil {
		return time.Time{}, "", err
	}
	return time.Now().Add(-duration), value, nil
}

func parseUsageDuration(value string) (time.Duration, error) {
	if strings.HasSuffix(value, "d") {
		days, err := strconv.Atoi(strings.TrimSuffix(value, "d"))
		if err != nil || days <= 0 {
			return 0, errors.New("since must be a duration like 24h, 7d, 30d, or all")
		}
		return time.Duration(days) * 24 * time.Hour, nil
	}
	duration, err := time.ParseDuration(value)
	if err != nil || duration <= 0 {
		return 0, errors.New("since must be a duration like 24h, 7d, 30d, or all")
	}
	return duration, nil
}

func parseUsageRecentLimit(value string) (int, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return 20, nil
	}
	limit, err := strconv.Atoi(value)
	if err != nil || limit < 0 {
		return 0, errors.New("recent must be a non-negative integer")
	}
	if limit > 200 {
		limit = 200
	}
	return limit, nil
}

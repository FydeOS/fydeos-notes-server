// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package graphql

import (
	"context"
	"errors"
	"net/http"
	"sort"
	"time"

	"github.com/fydeos/fydeos-notes-server/internal/graphql/model"
	"github.com/vektah/gqlparser/v2/gqlerror"
)

var ErrMailDeliveryAnalyticsUnavailable = errors.New("mail delivery analytics are unavailable")

type mailDeliveryWindow struct {
	from          time.Time
	to            time.Time
	bucket        model.TimeBucket
	requestedSize int
	effectiveSize int
}

func (w mailDeliveryWindow) databaseBucket() string {
	if w.bucket == model.TimeBucketDay {
		return "day"
	}
	return "hour"
}

type mailDeliverySeriesDefinition struct {
	key   string
	label string
}

type mailDeliverySeriesValue struct {
	key    string
	bucket string
}

func normalizeMailDeliveryWindow(hours int, now time.Time) (mailDeliveryWindow, error) {
	if hours != 24 && hours != 24*7 {
		return mailDeliveryWindow{}, errors.New("Mail delivery analytics window must be 24 or 168 hours.")
	}
	utc := now.UTC()
	window := mailDeliveryWindow{requestedSize: hours}
	if hours == 24 {
		window.bucket = model.TimeBucketHour
		window.effectiveSize = 24
		window.to = utc.Truncate(time.Hour).Add(time.Hour)
		window.from = window.to.Add(-24 * time.Hour)
		return window, nil
	}
	window.bucket = model.TimeBucketDay
	window.effectiveSize = 7
	window.to = time.Date(utc.Year(), utc.Month(), utc.Day()+1, 0, 0, 0, 0, time.UTC)
	window.from = window.to.AddDate(0, 0, -7)
	return window, nil
}

func (r *Resolver) adminMailDeliveries(ctx context.Context, input *model.AdminMailDeliveriesInput) (*model.AdminMailDeliveryAnalytics, error) {
	if err := requireAdministrator(ctx); err != nil {
		return nil, err
	}
	if r.MailDeliveryAnalytics == nil {
		return nil, ErrMailDeliveryAnalyticsUnavailable
	}
	hours := 24
	if input != nil {
		hours = input.Hours
	}
	window, err := normalizeMailDeliveryWindow(hours, time.Now())
	if err != nil {
		message := err.Error()
		return nil, &gqlerror.Error{Message: message, Extensions: userFriendlyGraphQLExtensions(http.StatusBadRequest, "bad_request", "bad_request", message, nil)}
	}
	rows, err := r.MailDeliveryAnalytics.AdminAggregate(ctx, window.from, window.to, window.databaseBucket())
	if err != nil {
		return nil, ErrMailDeliveryAnalyticsUnavailable
	}

	buckets := make([]time.Time, window.effectiveSize)
	for i := range buckets {
		if window.bucket == model.TimeBucketHour {
			buckets[i] = window.from.Add(time.Duration(i) * time.Hour)
		} else {
			buckets[i] = window.from.AddDate(0, 0, i)
		}
	}

	statusDefinitions := []mailDeliverySeriesDefinition{
		{key: "sent", label: "Sent"},
		{key: "failed", label: "Failed"},
		{key: "skipped", label: "Skipped"},
		{key: "canceled", label: "Canceled"},
		{key: "queued", label: "Queued"},
		{key: "sending", label: "Sending"},
		{key: "retry_wait", label: "Retry wait"},
	}
	outcomeDefinitions := []mailDeliverySeriesDefinition{
		{key: "successful", label: "Successful"},
		{key: "unsuccessful", label: "Unsuccessful"},
		{key: "pending", label: "Pending"},
	}
	statusCounts := make(map[mailDeliverySeriesValue]int)
	classCounts := make(map[mailDeliverySeriesValue]int)
	outcomeCounts := make(map[mailDeliverySeriesValue]int)
	classTotals := make(map[string]int)
	summary := &model.AdminMailDeliverySummary{}
	for _, row := range rows {
		bucket := row.Bucket.UTC().Format(time.RFC3339Nano)
		summary.Total += row.Count
		switch row.Status {
		case "sent":
			summary.Sent += row.Count
		case "failed":
			summary.Failed += row.Count
		case "skipped":
			summary.Skipped += row.Count
		case "canceled":
			summary.Canceled += row.Count
		case "queued":
			summary.Queued += row.Count
		case "sending":
			summary.Sending += row.Count
		case "retry_wait":
			summary.RetryWait += row.Count
		default:
			return nil, ErrMailDeliveryAnalyticsUnavailable
		}
		statusCounts[mailDeliverySeriesValue{key: row.Status, bucket: bucket}] += row.Count
		classCounts[mailDeliverySeriesValue{key: row.MailClass, bucket: bucket}] += row.Count
		classTotals[row.MailClass] += row.Count
		outcome := "unsuccessful"
		switch row.Status {
		case "sent":
			outcome = "successful"
		case "queued", "sending", "retry_wait":
			outcome = "pending"
		}
		outcomeCounts[mailDeliverySeriesValue{key: outcome, bucket: bucket}] += row.Count
	}
	terminal := summary.Sent + summary.Failed + summary.Skipped + summary.Canceled
	if terminal > 0 {
		summary.SuccessRate = float64(summary.Sent) / float64(terminal)
	}

	type mailClassTotal struct {
		key   string
		total int
	}
	topClasses := make([]mailClassTotal, 0, len(classTotals))
	for key, total := range classTotals {
		topClasses = append(topClasses, mailClassTotal{key: key, total: total})
	}
	sort.Slice(topClasses, func(i, j int) bool {
		if topClasses[i].total != topClasses[j].total {
			return topClasses[i].total > topClasses[j].total
		}
		return topClasses[i].key < topClasses[j].key
	})
	if len(topClasses) > 6 {
		topClasses = topClasses[:6]
	}
	typeDefinitions := make([]mailDeliverySeriesDefinition, len(topClasses))
	for i, class := range topClasses {
		typeDefinitions[i] = mailDeliverySeriesDefinition{key: class.key, label: class.key}
	}

	return &model.AdminMailDeliveryAnalytics{
		Window: &model.TimeWindow{
			From: window.from.Format(time.RFC3339Nano), To: window.to.Format(time.RFC3339Nano), Timezone: "UTC",
			Bucket: window.bucket, RequestedSize: window.requestedSize, EffectiveSize: window.effectiveSize,
		},
		Summary:   summary,
		ByStatus:  buildMailDeliverySeries(statusDefinitions, buckets, statusCounts),
		ByType:    buildMailDeliverySeries(typeDefinitions, buckets, classCounts),
		ByOutcome: buildMailDeliverySeries(outcomeDefinitions, buckets, outcomeCounts),
	}, nil
}

func buildMailDeliverySeries(definitions []mailDeliverySeriesDefinition, buckets []time.Time, counts map[mailDeliverySeriesValue]int) []*model.AdminMailDeliverySeries {
	series := make([]*model.AdminMailDeliverySeries, 0, len(definitions))
	for _, definition := range definitions {
		item := &model.AdminMailDeliverySeries{Key: definition.key, Label: definition.label, Points: make([]*model.AdminMailDeliveryPoint, 0, len(buckets))}
		for _, bucket := range buckets {
			formatted := bucket.UTC().Format(time.RFC3339Nano)
			count := counts[mailDeliverySeriesValue{key: definition.key, bucket: formatted}]
			item.Total += count
			item.Points = append(item.Points, &model.AdminMailDeliveryPoint{Bucket: formatted, Count: count})
		}
		series = append(series, item)
	}
	return series
}

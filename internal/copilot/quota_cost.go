// Copyright (c) 2026 Fyde Innovations Limited
// SPDX-License-Identifier: MIT

package copilot

import "context"

// countUserQuotaCost is shared by quota display and managed routing. Costs are
// lifetime units, with BYOK selection/token rows deduplicated by billing unit.
// A legacy prompt-action session counts once only when no action run exists.
func countUserQuotaCost(ctx context.Context, db quotaQueryer, userID string) (int64, error) {
	var used int64
	err := db.QueryRow(ctx, `WITH byok_usage AS (
		SELECT DISTINCT billing_unit_id,feature_kind FROM ai_usage_events
		WHERE user_id=$1 AND provider_source IN ('byok_server','byok_local')
		AND feature_kind IN ('chat','action','image','transcript') AND billing_unit_id IS NOT NULL
	), message_units AS (
		SELECT DISTINCT usage.billing_unit_id FROM byok_usage usage
		JOIN ai_sessions_messages message ON message.id=usage.billing_unit_id
		JOIN ai_sessions_metadata session ON session.id=message.session_id
		WHERE usage.feature_kind IN ('chat','action','image') AND message.role='user'
		AND session.user_id=$1 AND COALESCE(session.prompt_action,'')=''
	), action_units AS (
		SELECT DISTINCT usage.billing_unit_id FROM byok_usage usage
		JOIN ai_action_runs run ON run.id=usage.billing_unit_id
		WHERE usage.feature_kind IN ('action','image') AND run.user_id=$1
		AND run.status='succeeded' AND run.action_id NOT LIKE 'transcript.audio.%'
	), transcript_units AS (
		SELECT DISTINCT usage.billing_unit_id FROM byok_usage usage
		JOIN ai_transcript_tasks task ON task.id=usage.billing_unit_id
		WHERE usage.feature_kind='transcript' AND task.user_id=$1 AND task.status='settled'
	)
	SELECT GREATEST(0,
		(SELECT COALESCE(SUM("messageCost"),0) FROM ai_sessions_metadata WHERE user_id=$1 AND COALESCE(prompt_action,'')='')
		+ (SELECT COUNT(*) FROM ai_action_runs WHERE user_id=$1 AND status='succeeded' AND action_id NOT LIKE 'transcript.audio.%')
		+ (SELECT COUNT(*) FROM ai_sessions_metadata session WHERE user_id=$1 AND COALESCE(prompt_action,'')<>''
		   AND NOT EXISTS(SELECT 1 FROM ai_action_runs run WHERE run.session_id=session.id))
		+ (SELECT COUNT(*) FROM ai_transcript_tasks WHERE user_id=$1 AND status='settled')
		- (SELECT COUNT(*) FROM message_units) - (SELECT COUNT(*) FROM action_units) - (SELECT COUNT(*) FROM transcript_units)
		+ (SELECT COUNT(DISTINCT billing_unit_id) FROM ai_usage_events
		   WHERE user_id=$1 AND provider_source IN ('affine_cloud','affine_cloud_image_reserved')
		   AND feature_kind='image' AND billing_unit_id IS NOT NULL)
	)::bigint`, userID).Scan(&used)
	return used, err
}

// Copyright 2014 Canonical Ltd.
// Licensed under the AGPLv3, see LICENCE file for details.

package params

// Define the kinds to be included in stats keys.
const (
	StatsArchiveDownload     = "archive-download"
	StatsArchiveDelete       = "archive-delete"
	StatsArchiveFailedUpload = "archive-failed-upload"
	StatsArchiveUpload       = "archive-upload"
	// The following kinds are in use in the legacy API.
	StatsCharmInfo    = "charm-info"
	StatsCharmMissing = "charm-missing"
	StatsCharmEvent   = "charm-event"
)

// Statistic holds one element of a stats/counter
// response. See http://tinyurl.com/nkdovcf
type Statistic struct {
	Key   string `json:",omitempty"`
	Date  string `json:",omitempty"`
	Count int64
}

// StatsResponse holds the result of an id/meta/stats GET request.
// See http://tinyurl.com/lvyp2l5
type StatsResponse struct {
	// ArchiveDownloadCount is superceded by ArchiveDownload but maintained for
	// backward compatibility.
	ArchiveDownloadCount int64
	// ArchiveDownload holds the downloads count for a specific revision of the
	// entity.
	ArchiveDownload StatsCount
	// ArchiveDownloadAllRevisions holds the downloads count for all revisions
	// of the entity.
	ArchiveDownloadAllRevisions StatsCount
}

// StatsCount holds stats counts and is used as part of StatsResponse.
type StatsCount struct {
	Total int64 // Total count over all time.
	Day   int64 // Count over the last day.
	Week  int64 // Count over the last week.
	Month int64 // Count over the last month.
}

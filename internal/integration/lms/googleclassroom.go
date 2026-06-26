// Package lms provides LMS connectors for NovaGrade.
// Currently implemented: Google Classroom (mock-tested; live OAuth deferred).
package lms

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/azrtydxb/novagrade/internal/integration"
	"github.com/azrtydxb/novagrade/pkg/contracts"
)

const defaultBaseURL = "https://classroom.googleapis.com"

// Connector implements integration.RosterPuller and integration.GradePusher
// against the Google Classroom REST API.
//
// BaseURL is injectable for testing (set to an httptest.Server URL in tests;
// leave empty or set to the real Google endpoint in production).
//
// Token is the OAuth 2.0 Bearer access token. It must be supplied by the
// operator via the encrypted connection credentials
// (e.g. {"access_token": "ya29...."}).
//
// DEFERRED(phase4-live): operator supplies/refreshes the OAuth access_token
// via POST /v1/integrations; no OAuth flow built here. The token seam is the
// Token field: swap in a fresh token when the operator provides one.
//
// StudentName mapping for PushGrades: GradeRow.StudentName is treated as the
// Google Classroom userId (e.g. "uid-001"). The connector first fetches the
// studentSubmissions list for the configured courseWork and builds a
// userId→submissionId map, then PATCHes each submission. This avoids a
// separate student-lookup call. Operators must set StudentName to the userId
// (ExternalID) obtained from PullRoster.
type Connector struct {
	// BaseURL is the API root (e.g. "https://classroom.googleapis.com").
	// Defaults to defaultBaseURL when empty.
	BaseURL string

	// CourseID is the Google Classroom course ID (e.g. "123456789").
	CourseID string

	// CourseWorkID is the ID of the assignment to post grades against.
	CourseWorkID string

	// HTTPClient is the HTTP client to use for all API calls.
	// Defaults to http.DefaultClient when nil.
	HTTPClient *http.Client

	// Token is the OAuth 2.0 Bearer access token.
	// Never logged.
	Token string
}

func (c *Connector) baseURL() string {
	if c.BaseURL == "" {
		return defaultBaseURL
	}
	return c.BaseURL
}

func (c *Connector) client() *http.Client {
	if c.HTTPClient == nil {
		return http.DefaultClient
	}
	return c.HTTPClient
}

// doGet performs a GET request with the Bearer token and decodes the JSON response.
func (c *Connector) doGet(ctx context.Context, url string, dest any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("google classroom: build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)

	resp, err := c.client().Do(req)
	if err != nil {
		return fmt.Errorf("google classroom: GET %s: %w", url, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusUnauthorized {
		return authError(resp.StatusCode)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("google classroom: GET %s returned %d", url, resp.StatusCode)
	}

	if err := json.NewDecoder(resp.Body).Decode(dest); err != nil {
		return fmt.Errorf("google classroom: decode response: %w", err)
	}
	return nil
}

// doPatch performs a PATCH request with the Bearer token and decodes the JSON response.
func (c *Connector) doPatch(ctx context.Context, url string, body any) error {
	b, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("google classroom: marshal body: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPatch, url, strings.NewReader(string(b)))
	if err != nil {
		return fmt.Errorf("google classroom: build PATCH request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.client().Do(req)
	if err != nil {
		return fmt.Errorf("google classroom: PATCH %s: %w", url, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusUnauthorized {
		return authError(resp.StatusCode)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("google classroom: PATCH %s returned %d", url, resp.StatusCode)
	}
	return nil
}

// authError returns a clear authentication error.
func authError(code int) error {
	return fmt.Errorf("google classroom: auth error (%d): check OAuth access_token in connection credentials", code)
}

// --- Google Classroom API shapes ---

type gcStudent struct {
	UserID  string        `json:"userId"`
	Profile gcUserProfile `json:"profile"`
}

type gcUserProfile struct {
	EmailAddress string `json:"emailAddress"`
	Name         gcName `json:"name"`
}

type gcName struct {
	FullName string `json:"fullName"`
}

type gcStudentsListResp struct {
	Students      []gcStudent `json:"students"`
	NextPageToken string      `json:"nextPageToken"`
}

type gcStudentSubmission struct {
	ID     string `json:"id"`
	UserID string `json:"userId"`
	State  string `json:"state"`
}

type gcSubmissionsListResp struct {
	StudentSubmissions []gcStudentSubmission `json:"studentSubmissions"`
	NextPageToken      string                `json:"nextPageToken"`
}

// PullRoster implements integration.RosterPuller.
//
// It calls GET /v1/courses/{courseId}/students (paginated) and returns the
// full student list as []contracts.RosterStudent. The io.Reader-based
// RosterSource interface is NOT implemented: Google Classroom's roster comes
// from an API call, not a file, so the file-parser contract would be a
// semantic mismatch.
func (c *Connector) PullRoster(ctx context.Context) ([]contracts.RosterStudent, error) {
	var students []contracts.RosterStudent
	pageToken := ""

	for {
		url := fmt.Sprintf("%s/v1/courses/%s/students", c.baseURL(), c.CourseID)
		if pageToken != "" {
			url += "?pageToken=" + pageToken
		}

		var page gcStudentsListResp
		if err := c.doGet(ctx, url, &page); err != nil {
			return nil, err
		}

		for _, s := range page.Students {
			students = append(students, contracts.RosterStudent{
				Email:      s.Profile.EmailAddress,
				FullName:   s.Profile.Name.FullName,
				ExternalID: s.UserID,
			})
		}

		if page.NextPageToken == "" {
			break
		}
		pageToken = page.NextPageToken
	}

	return students, nil
}

// PushGrades implements integration.GradePusher.
//
// Simplification: GradeRow.StudentName is expected to carry the Google
// Classroom userId (the ExternalID from PullRoster). The connector first
// fetches the studentSubmissions list for the configured courseWork, builds
// a userId→submissionId map, then PATCHes each submission's assignedGrade.
// Per-row errors are collected; if any row fails the method returns a combined
// error after attempting all rows.
func (c *Connector) PushGrades(ctx context.Context, rows []contracts.GradeRow) error {
	// 1. Fetch submission list to build userId → submissionId map.
	subURL := fmt.Sprintf("%s/v1/courses/%s/courseWork/%s/studentSubmissions",
		c.baseURL(), c.CourseID, c.CourseWorkID)

	var subResp gcSubmissionsListResp
	if err := c.doGet(ctx, subURL, &subResp); err != nil {
		return fmt.Errorf("google classroom: fetch submissions: %w", err)
	}

	submissionByUser := make(map[string]string, len(subResp.StudentSubmissions))
	for _, sub := range subResp.StudentSubmissions {
		submissionByUser[sub.UserID] = sub.ID
	}

	// 2. PATCH each grade row.
	var errs []string
	for _, row := range rows {
		subID, ok := submissionByUser[row.StudentName]
		if !ok {
			errs = append(errs, fmt.Sprintf("no submission found for userId %q", row.StudentName))
			continue
		}

		patchURL := fmt.Sprintf("%s/v1/courses/%s/courseWork/%s/studentSubmissions/%s?updateMask=assignedGrade",
			c.baseURL(), c.CourseID, c.CourseWorkID, subID)

		payload := map[string]any{"assignedGrade": row.Awarded}
		if err := c.doPatch(ctx, patchURL, payload); err != nil {
			errs = append(errs, fmt.Sprintf("userId %q sub %q: %v", row.StudentName, subID, err))
		}
	}

	if len(errs) > 0 {
		return errors.New("google classroom: push grades errors: " + strings.Join(errs, "; "))
	}
	return nil
}

// Register wires the Google Classroom connector into reg under (CategoryLMS, "google_classroom").
//
// The factory returns a *Connector with default BaseURL; callers must set
// CourseID, CourseWorkID, and Token from the decrypted connection credentials
// before using the returned connector.
func Register(reg *integration.Registry) {
	reg.Register(integration.CategoryLMS, "google_classroom", func() any {
		return &Connector{BaseURL: defaultBaseURL}
	})
}

package lms_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/azrtydxb/novagrade/internal/integration"
	"github.com/azrtydxb/novagrade/internal/integration/lms"
	"github.com/azrtydxb/novagrade/pkg/contracts"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testToken = "test-bearer-token-abc123"

// --- PullRoster tests ---

// TestPullRoster_SinglePage verifies that a single-page students.list response
// is correctly parsed into []RosterStudent and that the bearer token is sent.
func TestPullRoster_SinglePage(t *testing.T) {
	resp := map[string]any{
		"students": []map[string]any{
			{
				"userId": "uid-001",
				"profile": map[string]any{
					"emailAddress": "alice@school.example",
					"name":         map[string]any{"fullName": "Alice Smith"},
				},
			},
			{
				"userId": "uid-002",
				"profile": map[string]any{
					"emailAddress": "bob@school.example",
					"name":         map[string]any{"fullName": "Bob Jones"},
				},
			},
		},
	}

	var gotAuthHeader string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuthHeader = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	c := lms.Connector{
		BaseURL:    srv.URL,
		CourseID:   "course-101",
		HTTPClient: srv.Client(),
		Token:      testToken,
	}

	students, err := c.PullRoster(context.Background())
	require.NoError(t, err)
	require.Len(t, students, 2)

	assert.Equal(t, "Bearer "+testToken, gotAuthHeader)

	assert.Equal(t, contracts.RosterStudent{Email: "alice@school.example", FullName: "Alice Smith", ExternalID: "uid-001"}, students[0])
	assert.Equal(t, contracts.RosterStudent{Email: "bob@school.example", FullName: "Bob Jones", ExternalID: "uid-002"}, students[1])
}

// TestPullRoster_Pagination verifies that nextPageToken is followed and results
// across pages are concatenated.
func TestPullRoster_Pagination(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		pageToken := r.URL.Query().Get("pageToken")
		w.Header().Set("Content-Type", "application/json")

		if pageToken == "" {
			// First page
			json.NewEncoder(w).Encode(map[string]any{
				"students": []map[string]any{
					{"userId": "uid-001", "profile": map[string]any{
						"emailAddress": "alice@school.example",
						"name":         map[string]any{"fullName": "Alice Smith"},
					}},
				},
				"nextPageToken": "page2token",
			})
		} else {
			// Second page
			json.NewEncoder(w).Encode(map[string]any{
				"students": []map[string]any{
					{"userId": "uid-002", "profile": map[string]any{
						"emailAddress": "bob@school.example",
						"name":         map[string]any{"fullName": "Bob Jones"},
					}},
				},
			})
		}
	}))
	defer srv.Close()

	c := lms.Connector{
		BaseURL:    srv.URL,
		CourseID:   "course-101",
		HTTPClient: srv.Client(),
		Token:      testToken,
	}

	students, err := c.PullRoster(context.Background())
	require.NoError(t, err)
	require.Len(t, students, 2, "expected 2 students across 2 pages")
	assert.Equal(t, 2, calls, "expected 2 HTTP calls (one per page)")
	assert.Equal(t, "alice@school.example", students[0].Email)
	assert.Equal(t, "bob@school.example", students[1].Email)
}

// TestPullRoster_401 verifies that a 401 from the mock produces a clear auth error.
func TestPullRoster_401(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"error":{"code":401,"message":"Request had invalid authentication credentials."}}`))
	}))
	defer srv.Close()

	c := lms.Connector{
		BaseURL:    srv.URL,
		CourseID:   "course-101",
		HTTPClient: srv.Client(),
		Token:      "bad-token",
	}

	_, err := c.PullRoster(context.Background())
	require.Error(t, err)
	assert.True(t, strings.Contains(strings.ToLower(err.Error()), "auth") || strings.Contains(err.Error(), "401"),
		"expected auth/401 error, got: %v", err)
}

// --- PushGrades tests ---

// TestPushGrades_Success verifies that PushGrades issues the expected PATCH
// requests for each grade row and includes the bearer token.
func TestPushGrades_Success(t *testing.T) {
	// Simulate the Classroom API:
	// GET .../studentSubmissions → return list with one submission per student
	// PATCH .../studentSubmissions/{id}?updateMask=assignedGrade → record it
	type patchRecord struct {
		submissionID  string
		assignedGrade float64
	}
	var patches []patchRecord
	var gotAuthHeaders []string

	// Two students: alice maps to submission "sub-a", bob to "sub-b"
	subList := map[string]any{
		"studentSubmissions": []map[string]any{
			{
				"id":     "sub-a",
				"userId": "uid-001",
				"state":  "TURNED_IN",
			},
			{
				"id":     "sub-b",
				"userId": "uid-002",
				"state":  "TURNED_IN",
			},
		},
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuthHeaders = append(gotAuthHeaders, r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "application/json")

		if r.Method == http.MethodGet && strings.Contains(r.URL.Path, "studentSubmissions") {
			json.NewEncoder(w).Encode(subList)
			return
		}

		if r.Method == http.MethodPatch && strings.Contains(r.URL.Path, "studentSubmissions") {
			// Extract submission ID from path: .../studentSubmissions/{id}
			parts := strings.Split(r.URL.Path, "/")
			subID := parts[len(parts)-1]
			assert.Equal(t, "application/json", r.Header.Get("Content-Type"), "PATCH must send Content-Type: application/json")
			var body map[string]any
			json.NewDecoder(r.Body).Decode(&body)
			grade, _ := body["assignedGrade"].(float64)
			patches = append(patches, patchRecord{submissionID: subID, assignedGrade: grade})
			json.NewEncoder(w).Encode(map[string]any{"id": subID, "assignedGrade": grade})
			return
		}

		http.NotFound(w, r)
	}))
	defer srv.Close()

	c := lms.Connector{
		BaseURL:      srv.URL,
		CourseID:     "course-101",
		CourseWorkID: "cw-42",
		HTTPClient:   srv.Client(),
		Token:        testToken,
	}

	rows := []contracts.GradeRow{
		{StudentName: "uid-001", Awarded: 85, MaxMarks: 100},
		{StudentName: "uid-002", Awarded: 72, MaxMarks: 100},
	}

	err := c.PushGrades(context.Background(), rows)
	require.NoError(t, err)

	require.Len(t, patches, 2)
	// Verify grades were sent (order may vary; map by submissionID)
	gradeByID := make(map[string]float64)
	for _, p := range patches {
		gradeByID[p.submissionID] = p.assignedGrade
	}
	assert.Equal(t, 85.0, gradeByID["sub-a"])
	assert.Equal(t, 72.0, gradeByID["sub-b"])

	// All requests must carry the bearer token
	for _, hdr := range gotAuthHeaders {
		assert.Equal(t, "Bearer "+testToken, hdr)
	}
}

// TestPushGrades_RowError verifies that a 500 on one PATCH surfaces a per-row error.
func TestPushGrades_RowError(t *testing.T) {
	subList := map[string]any{
		"studentSubmissions": []map[string]any{
			{"id": "sub-a", "userId": "uid-001", "state": "TURNED_IN"},
		},
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet {
			json.NewEncoder(w).Encode(subList)
			return
		}
		// PATCH → 500
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(`{"error":{"message":"internal"}}`))
	}))
	defer srv.Close()

	c := lms.Connector{
		BaseURL:      srv.URL,
		CourseID:     "course-101",
		CourseWorkID: "cw-42",
		HTTPClient:   srv.Client(),
		Token:        testToken,
	}

	rows := []contracts.GradeRow{
		{StudentName: "uid-001", Awarded: 90, MaxMarks: 100},
	}

	err := c.PushGrades(context.Background(), rows)
	require.Error(t, err, "expected an error when PATCH returns 500")
}

// TestPushGrades_401 verifies that a 401 on the submission list produces a clear auth error.
func TestPushGrades_401(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		fmt.Fprint(w, `{"error":{"code":401,"message":"invalid credentials"}}`)
	}))
	defer srv.Close()

	c := lms.Connector{
		BaseURL:      srv.URL,
		CourseID:     "course-101",
		CourseWorkID: "cw-42",
		HTTPClient:   srv.Client(),
		Token:        "bad",
	}

	err := c.PushGrades(context.Background(), []contracts.GradeRow{{StudentName: "uid-001", Awarded: 50, MaxMarks: 100}})
	require.Error(t, err)
	assert.True(t, strings.Contains(strings.ToLower(err.Error()), "auth") || strings.Contains(err.Error(), "401"),
		"expected auth/401 error, got: %v", err)
}

// TestPushGrades_SubmissionPagination verifies that PushGrades follows nextPageToken
// across multiple pages of studentSubmissions before PATCHing, so that a student
// whose submission appears only on the SECOND page is not silently dropped.
func TestPushGrades_SubmissionPagination(t *testing.T) {
	type patchRecord struct {
		submissionID  string
		assignedGrade float64
	}
	var patches []patchRecord
	calls := 0

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		if r.Method == http.MethodGet && strings.Contains(r.URL.Path, "studentSubmissions") {
			calls++
			pageToken := r.URL.Query().Get("pageToken")
			if pageToken == "" {
				// First page: uid-001 only; signal a second page.
				json.NewEncoder(w).Encode(map[string]any{
					"studentSubmissions": []map[string]any{
						{"id": "sub-a", "userId": "uid-001", "state": "TURNED_IN"},
					},
					"nextPageToken": "page2token",
				})
			} else {
				// Second page: uid-002.
				json.NewEncoder(w).Encode(map[string]any{
					"studentSubmissions": []map[string]any{
						{"id": "sub-b", "userId": "uid-002", "state": "TURNED_IN"},
					},
				})
			}
			return
		}

		if r.Method == http.MethodPatch && strings.Contains(r.URL.Path, "studentSubmissions") {
			parts := strings.Split(r.URL.Path, "/")
			subID := parts[len(parts)-1]
			var body map[string]any
			json.NewDecoder(r.Body).Decode(&body)
			grade, _ := body["assignedGrade"].(float64)
			patches = append(patches, patchRecord{submissionID: subID, assignedGrade: grade})
			json.NewEncoder(w).Encode(map[string]any{"id": subID, "assignedGrade": grade})
			return
		}

		http.NotFound(w, r)
	}))
	defer srv.Close()

	c := lms.Connector{
		BaseURL:      srv.URL,
		CourseID:     "course-101",
		CourseWorkID: "cw-42",
		HTTPClient:   srv.Client(),
		Token:        testToken,
	}

	rows := []contracts.GradeRow{
		{StudentName: "uid-001", Awarded: 85, MaxMarks: 100},
		{StudentName: "uid-002", Awarded: 72, MaxMarks: 100}, // lives on page 2
	}

	err := c.PushGrades(context.Background(), rows)
	require.NoError(t, err, "expected no error: uid-002 is on page 2 and must be found")

	assert.Equal(t, 2, calls, "expected 2 GET calls (one per submissions page)")

	require.Len(t, patches, 2, "expected 2 PATCHes: one per student")
	gradeByID := make(map[string]float64)
	for _, p := range patches {
		gradeByID[p.submissionID] = p.assignedGrade
	}
	assert.Equal(t, 85.0, gradeByID["sub-a"], "uid-001 grade")
	assert.Equal(t, 72.0, gradeByID["sub-b"], "uid-002 grade (from page 2)")
}

// --- Registry test ---

// TestRegister verifies that Register wires the Connector into the registry
// and that it satisfies RosterPuller and GradePusher.
func TestRegister(t *testing.T) {
	reg := integration.NewRegistry()
	lms.Register(reg)

	got, ok := reg.Get(integration.CategoryLMS, "google_classroom")
	require.True(t, ok, "expected (CategoryLMS, 'google_classroom') to be registered")

	_, isPuller := got.(integration.RosterPuller)
	assert.True(t, isPuller, "expected RosterPuller, got %T", got)

	_, isPusher := got.(integration.GradePusher)
	assert.True(t, isPusher, "expected GradePusher, got %T", got)
}

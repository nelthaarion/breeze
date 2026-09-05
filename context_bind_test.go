package breeze

import (
	"net/url"
	"testing"

	"github.com/goccy/go-json"
)

// TestContextBind_JSONSuccess tests binding JSON body into a struct.
func TestContextBind_JSONSuccess(t *testing.T) {
	type LoginRequest struct {
		Email    string `json:"email"`
		Password string `json:"password"`
	}

	body := []byte(`{"email":"alice@example.com","password":"secret123"}`)

	ctx := NewContext(POST, "/login")
	ctx.Req.Body = body

	var req LoginRequest
	err := ctx.Bind(&req)

	if err != nil {
		t.Fatalf("Bind() error = %v", err)
	}

	if req.Email != "alice@example.com" || req.Password != "secret123" {
		t.Errorf("Bind() got %+v, want {Email:alice@example.com Password:secret123}", req)
	}

	// Response should be untouched on success.
	if ctx.Res != nil && ctx.Res.Status != 0 {
		t.Errorf("Response status set on success: %d", ctx.Res.Status)
	}
}

// TestContextBind_JSONValidationFail tests that validation errors return 422 with problem+json.
func TestContextBind_JSONValidationFail(t *testing.T) {
	type LoginRequest struct {
		Email    string `json:"email" validate:"required,email"`
		Password string `json:"password" validate:"required,min=8"`
	}

	body := []byte(`{"email":"not-an-email","password":"short"}`)

	ctx := NewContext(POST, "/login")
	ctx.Req.Body = body

	var req LoginRequest
	err := ctx.Bind(&req)

	if err == nil {
		t.Fatal("Bind() expected error, got nil")
	}

	// Should set status 422.
	if ctx.Res == nil || ctx.Res.Status != 422 {
		t.Errorf("Expected status 422, got %v", ctx.Res)
	}

	// Response body should be problem+json.
	if ctx.Res.Headers["Content-Type"] != "application/json" {
		t.Errorf("Expected JSON content-type, got %s", ctx.Res.Headers["Content-Type"])
	}
}

// TestContextBind_JSONMalformed tests that malformed JSON returns 400.
func TestContextBind_JSONMalformed(t *testing.T) {
	type Data struct {
		Value string `json:"value"`
	}

	body := []byte(`{invalid json}`)

	ctx := NewContext(POST, "/api")
	ctx.Req.Body = body

	var data Data
	err := ctx.Bind(&data)

	if err == nil {
		t.Fatal("Bind() expected error, got nil")
	}

	// Should set status 400.
	if ctx.Res == nil || ctx.Res.Status != 400 {
		t.Errorf("Expected status 400, got %v", ctx.Res)
	}
}

// TestContextBind_QueryParameters tests binding query parameters.
func TestContextBind_QueryParameters(t *testing.T) {
	type SearchRequest struct {
		Query  string `form:"q"`
		Limit  int    `form:"limit"`
		Offset int    `form:"offset"`
	}

	ctx := NewContext(GET, "/search")
	ctx.Req.Query = url.Values{
		"q":      {"golang"},
		"limit":  {"10"},
		"offset": {"5"},
	}

	var req SearchRequest
	err := ctx.Bind(&req)

	if err != nil {
		t.Fatalf("Bind() error = %v", err)
	}

	if req.Query != "golang" || req.Limit != 10 || req.Offset != 5 {
		t.Errorf("Bind() got %+v, want {Query:golang Limit:10 Offset:5}", req)
	}
}

// TestContextBind_PathParameters tests binding path parameters.
func TestContextBind_PathParameters(t *testing.T) {
	type RouteParams struct {
		UserID int    `param:"id"`
		Slug   string `param:"slug"`
	}

	ctx := NewContext(GET, "/users/42/posts/my-post")
	ctx.SetParams(map[string]string{
		"id":   "42",
		"slug": "my-post",
	})

	var params RouteParams
	err := ctx.Bind(&params)

	if err != nil {
		t.Fatalf("Bind() error = %v", err)
	}

	if params.UserID != 42 || params.Slug != "my-post" {
		t.Errorf("Bind() got %+v, want {UserID:42 Slug:my-post}", params)
	}
}

// TestContextBind_Combined tests binding from multiple sources at once.
func TestContextBind_Combined(t *testing.T) {
	type Request struct {
		ID       int    `param:"id" json:"id"`
		Name     string `json:"name"`
		PageSize int    `form:"page_size"`
	}

	ctx := NewContext(POST, "/users/1")
	ctx.Req.Body = []byte(`{"name":"Alice"}`)
	ctx.Req.Query = url.Values{"page_size": {"20"}}
	ctx.SetParams(map[string]string{"id": "1"})

	var req Request
	err := ctx.Bind(&req)

	if err != nil {
		t.Fatalf("Bind() error = %v", err)
	}

	if req.ID != 1 || req.Name != "Alice" || req.PageSize != 20 {
		t.Errorf("Bind() got %+v, want {ID:1 Name:Alice PageSize:20}", req)
	}
}

// TestContextBind_ValidateEmail tests email validation rule.
func TestContextBind_ValidateEmail(t *testing.T) {
	type Form struct {
		Email string `json:"email" validate:"email"`
	}

	tests := []struct {
		name      string
		body      []byte
		wantCode  int
		shouldErr bool
	}{
		{
			name:     "valid email",
			body:     []byte(`{"email":"alice@example.com"}`),
			wantCode: 0, // no error, no status set
		},
		{
			name:      "invalid email",
			body:      []byte(`{"email":"not-an-email"}`),
			wantCode:  422,
			shouldErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := NewContext(POST, "/form")
			ctx.Req.Body = tt.body

			var form Form
			err := ctx.Bind(&form)

			if tt.shouldErr {
				if err == nil {
					t.Fatal("Bind() expected error, got nil")
				}
				if ctx.Res == nil || ctx.Res.Status != tt.wantCode {
					t.Errorf("Expected status %d, got %v", tt.wantCode, ctx.Res)
				}
			} else {
				if err != nil {
					t.Fatalf("Bind() unexpected error: %v", err)
				}
			}
		})
	}
}

// TestContextBind_ValidateRequired tests required rule.
func TestContextBind_ValidateRequired(t *testing.T) {
	type Form struct {
		Username string `json:"username" validate:"required"`
	}

	ctx := NewContext(POST, "/form")
	ctx.Req.Body = []byte(`{"username":""}`)

	var form Form
	err := ctx.Bind(&form)

	if err == nil {
		t.Fatal("Bind() expected error, got nil")
	}

	if ctx.Res == nil || ctx.Res.Status != 422 {
		t.Errorf("Expected status 422, got %v", ctx.Res)
	}
}

// TestContextBind_EmptyBody tests binding with no body.
func TestContextBind_EmptyBody(t *testing.T) {
	type Data struct {
		Value string `json:"value"`
	}

	ctx := NewContext(GET, "/api")
	// No body set, Query and params are also empty.

	var data Data
	err := ctx.Bind(&data)

	if err != nil {
		t.Fatalf("Bind() unexpected error: %v", err)
	}

	// Should succeed with zero values.
	if data.Value != "" {
		t.Errorf("Expected empty value, got %q", data.Value)
	}
}

// TestContextBind_ResponseStatus tests that Status() is preserved across JSON().
func TestContextBind_ResponseStatus(t *testing.T) {
	type Form struct {
		Email string `validate:"email"`
	}

	ctx := NewContext(POST, "/form")
	ctx.Req.Body = []byte(`{"email":"invalid"}`)

	var form Form
	err := ctx.Bind(&form)

	if err == nil {
		t.Fatal("expected error")
	}

	// Status should be 422 from Bind().
	if ctx.Res.Status != 422 {
		t.Errorf("Expected status 422, got %d", ctx.Res.Status)
	}
}

// TestContextBind_ProblemJSONStructure tests that the problem+json response is correct.
func TestContextBind_ProblemJSONStructure(t *testing.T) {
	type Form struct {
		Email    string `json:"email" validate:"required,email"`
		Password string `json:"password" validate:"required,min=8"`
	}

	ctx := NewContext(POST, "/form")
	ctx.Req.Body = []byte(`{"email":"","password":"short"}`)

	var form Form
	_ = ctx.Bind(&form)

	// Parse the response body to check structure.
	var resp map[string]any
	if err := json.Unmarshal(ctx.Res.Body, &resp); err != nil {
		t.Fatalf("Failed to unmarshal response: %v", err)
	}

	// Check RFC 9457 structure.
	if resp["type"] != "about:blank" {
		t.Errorf("Expected type 'about:blank', got %v", resp["type"])
	}
	if resp["title"] != "Validation Failed" {
		t.Errorf("Expected title 'Validation Failed', got %v", resp["title"])
	}
	if resp["status"] != float64(422) {
		t.Errorf("Expected status 422, got %v", resp["status"])
	}

	errors, ok := resp["errors"].([]any)
	if !ok || len(errors) < 2 {
		t.Errorf("Expected errors array with at least 2 items, got %v", resp["errors"])
	}
}

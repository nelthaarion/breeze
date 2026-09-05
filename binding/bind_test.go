package binding

import (
	"net/url"
	"reflect"
	"testing"
)

// ─── Test helpers ─────────────────────────────────────────────────────────

func TestBind_JSONBody(t *testing.T) {
	type User struct {
		Name  string `json:"name"`
		Email string `json:"email"`
	}

	tests := []struct {
		name    string
		body    []byte
		want    *User
		wantErr bool
	}{
		{
			name: "valid json",
			body: []byte(`{"name":"Alice","email":"alice@example.com"}`),
			want: &User{Name: "Alice", Email: "alice@example.com"},
		},
		{
			name: "empty body",
			body: []byte{},
			want: &User{},
		},
		{
			name:    "malformed json",
			body:    []byte(`{invalid}`),
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var u User
			err := Bind(&u, JSONBody(tt.body))

			if (err != nil) != tt.wantErr {
				t.Errorf("Bind() error = %v, wantErr %v", err, tt.wantErr)
				return
			}

			if !tt.wantErr {
				if u.Name != tt.want.Name || u.Email != tt.want.Email {
					t.Errorf("Bind() got %+v, want %+v", u, tt.want)
				}
			}
		})
	}
}

func TestBind_QueryParameters(t *testing.T) {
	type Filters struct {
		Limit  int    `form:"limit"`
		Offset int    `form:"offset"`
		Query  string `form:"q"`
	}

	tests := []struct {
		name    string
		values  url.Values
		want    Filters
		wantErr bool
	}{
		{
			name: "valid query",
			values: url.Values{
				"limit":  {"10"},
				"offset": {"5"},
				"q":      {"search term"},
			},
			want: Filters{Limit: 10, Offset: 5, Query: "search term"},
		},
		{
			name:   "empty query",
			values: url.Values{},
			want:   Filters{},
		},
		{
			name: "invalid integer",
			values: url.Values{
				"limit": {"not-a-number"},
			},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var f Filters
			err := Bind(&f, Query(tt.values))

			if (err != nil) != tt.wantErr {
				t.Errorf("Bind() error = %v, wantErr %v", err, tt.wantErr)
				return
			}

			if !tt.wantErr {
				if f != tt.want {
					t.Errorf("Bind() got %+v, want %+v", f, tt.want)
				}
			}
		})
	}
}

func TestBind_PathParameters(t *testing.T) {
	type Route struct {
		ID   int    `param:"id"`
		Slug string `param:"slug"`
	}

	tests := []struct {
		name    string
		params  map[string]string
		want    Route
		wantErr bool
	}{
		{
			name: "valid params",
			params: map[string]string{
				"id":   "42",
				"slug": "my-post",
			},
			want: Route{ID: 42, Slug: "my-post"},
		},
		{
			name:   "empty params",
			params: map[string]string{},
			want:   Route{},
		},
		{
			name: "invalid integer",
			params: map[string]string{
				"id": "not-a-number",
			},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var r Route
			err := Bind(&r, Path(tt.params))

			if (err != nil) != tt.wantErr {
				t.Errorf("Bind() error = %v, wantErr %v", err, tt.wantErr)
				return
			}

			if !tt.wantErr {
				if r != tt.want {
					t.Errorf("Bind() got %+v, want %+v", r, tt.want)
				}
			}
		})
	}
}

func TestBind_MultipleSources(t *testing.T) {
	type Request struct {
		ID    int    `param:"id" json:"id"`
		Name  string `           json:"name"`
		Limit int    `                       form:"limit"`
	}

	body := []byte(`{"name":"Alice"}`)
	values := url.Values{"limit": {"20"}}
	params := map[string]string{"id": "1"}

	var r Request
	err := Bind(&r, JSONBody(body), Query(values), Path(params))
	if err != nil {
		t.Fatalf("Bind() error = %v", err)
	}

	if r.ID != 1 || r.Name != "Alice" || r.Limit != 20 {
		t.Errorf("Bind() got %+v, want {ID:1 Name:Alice Limit:20}", r)
	}
}

// ─── Validation tests ─────────────────────────────────────────────────────

func TestValidate_Required(t *testing.T) {
	type Form struct {
		Name string `validate:"required"`
	}

	tests := []struct {
		name    string
		input   Form
		wantErr bool
	}{
		{name: "filled", input: Form{Name: "Alice"}, wantErr: false},
		{name: "empty", input: Form{Name: ""}, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := tt.input
			// Direct validation by binding with empty sources.
			err := Bind(&f)

			if (err != nil) != tt.wantErr {
				t.Errorf("Bind() error = %v, wantErr %v", err, tt.wantErr)
			}

			if tt.wantErr {
				verr, ok := err.(*ValidationError)
				if !ok {
					t.Errorf("expected *ValidationError, got %T", err)
					return
				}
				if len(verr.Errors) != 1 || verr.Errors[0].Rule != "required" {
					t.Errorf("expected required rule error, got %+v", verr.Errors)
				}
			}
		})
	}
}

func TestValidate_Min(t *testing.T) {
	type FormInt struct {
		Count int `validate:"min=5"`
	}

	type FormStr struct {
		Name string `validate:"min=3"`
	}

	tests := []struct {
		name   string
		testFn func(*testing.T)
	}{
		{
			name: "int passes",
			testFn: func(t *testing.T) {
				f := FormInt{Count: 10}
				if err := Bind(&f); err != nil {
					t.Errorf("Bind() error = %v, want nil", err)
				}
			},
		},
		{
			name: "int fails",
			testFn: func(t *testing.T) {
				f := FormInt{Count: 3}
				err := Bind(&f)
				if err == nil {
					t.Error("Bind() expected error, got nil")
					return
				}
				verr, ok := err.(*ValidationError)
				if !ok || len(verr.Errors) == 0 || verr.Errors[0].Rule != "min" {
					t.Errorf("expected min rule error, got %+v", err)
				}
			},
		},
		{
			name: "string passes",
			testFn: func(t *testing.T) {
				f := FormStr{Name: "Alice"}
				if err := Bind(&f); err != nil {
					t.Errorf("Bind() error = %v, want nil", err)
				}
			},
		},
		{
			name: "string fails",
			testFn: func(t *testing.T) {
				f := FormStr{Name: "Al"}
				err := Bind(&f)
				if err == nil {
					t.Error("Bind() expected error, got nil")
					return
				}
				verr, ok := err.(*ValidationError)
				if !ok || len(verr.Errors) == 0 || verr.Errors[0].Rule != "min" {
					t.Errorf("expected min rule error, got %+v", err)
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, tt.testFn)
	}
}

func TestValidate_Max(t *testing.T) {
	type FormInt struct {
		Count int `validate:"max=100"`
	}

	type FormStr struct {
		Name string `validate:"max=10"`
	}

	tests := []struct {
		name   string
		testFn func(*testing.T)
	}{
		{
			name: "int passes",
			testFn: func(t *testing.T) {
				f := FormInt{Count: 50}
				if err := Bind(&f); err != nil {
					t.Errorf("Bind() error = %v, want nil", err)
				}
			},
		},
		{
			name: "int fails",
			testFn: func(t *testing.T) {
				f := FormInt{Count: 150}
				err := Bind(&f)
				if err == nil {
					t.Error("Bind() expected error, got nil")
					return
				}
				verr, ok := err.(*ValidationError)
				if !ok || len(verr.Errors) == 0 || verr.Errors[0].Rule != "max" {
					t.Errorf("expected max rule error, got %+v", err)
				}
			},
		},
		{
			name: "string passes",
			testFn: func(t *testing.T) {
				f := FormStr{Name: "Alice"}
				if err := Bind(&f); err != nil {
					t.Errorf("Bind() error = %v, want nil", err)
				}
			},
		},
		{
			name: "string fails",
			testFn: func(t *testing.T) {
				f := FormStr{Name: "VeryLongName"}
				err := Bind(&f)
				if err == nil {
					t.Error("Bind() expected error, got nil")
					return
				}
				verr, ok := err.(*ValidationError)
				if !ok || len(verr.Errors) == 0 || verr.Errors[0].Rule != "max" {
					t.Errorf("expected max rule error, got %+v", err)
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, tt.testFn)
	}
}

func TestValidate_Email(t *testing.T) {
	type Form struct {
		Email string `validate:"email"`
	}

	tests := []struct {
		name   string
		testFn func(*testing.T)
	}{
		{
			name: "valid email",
			testFn: func(t *testing.T) {
				f := Form{Email: "alice@example.com"}
				if err := Bind(&f); err != nil {
					t.Errorf("Bind() error = %v, want nil", err)
				}
			},
		},
		{
			name: "invalid email",
			testFn: func(t *testing.T) {
				f := Form{Email: "not-an-email"}
				err := Bind(&f)
				if err == nil {
					t.Error("Bind() expected error, got nil")
					return
				}
				verr, ok := err.(*ValidationError)
				if !ok || len(verr.Errors) == 0 || verr.Errors[0].Rule != "email" {
					t.Errorf("expected email rule error, got %+v", err)
				}
			},
		},
		{
			name: "empty email",
			testFn: func(t *testing.T) {
				f := Form{Email: ""}
				if err := Bind(&f); err != nil {
					t.Errorf("Bind() error = %v, want nil (empty optional field)", err)
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, tt.testFn)
	}
}

func TestValidate_Oneof(t *testing.T) {
	type Form struct {
		Status string `validate:"oneof=active inactive pending"`
	}

	tests := []struct {
		name   string
		testFn func(*testing.T)
	}{
		{
			name: "valid value",
			testFn: func(t *testing.T) {
				f := Form{Status: "active"}
				if err := Bind(&f); err != nil {
					t.Errorf("Bind() error = %v, want nil", err)
				}
			},
		},
		{
			name: "invalid value",
			testFn: func(t *testing.T) {
				f := Form{Status: "unknown"}
				err := Bind(&f)
				if err == nil {
					t.Error("Bind() expected error, got nil")
					return
				}
				verr, ok := err.(*ValidationError)
				if !ok || len(verr.Errors) == 0 || verr.Errors[0].Rule != "oneof" {
					t.Errorf("expected oneof rule error, got %+v", err)
				}
			},
		},
		{
			name: "empty value",
			testFn: func(t *testing.T) {
				f := Form{Status: ""}
				if err := Bind(&f); err != nil {
					t.Errorf("Bind() error = %v, want nil (empty optional field)", err)
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, tt.testFn)
	}
}

func TestValidate_MultipleRules(t *testing.T) {
	type Form struct {
		Email string `validate:"required,email"`
	}

	tests := []struct {
		name       string
		input      Form
		wantErrors int
	}{
		{name: "valid", input: Form{Email: "alice@example.com"}, wantErrors: 0},
		{name: "empty", input: Form{Email: ""}, wantErrors: 1},
		{name: "invalid email", input: Form{Email: "not-email"}, wantErrors: 1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := tt.input
			err := Bind(&f)

			if tt.wantErrors == 0 {
				if err != nil {
					t.Errorf("Bind() error = %v, want nil", err)
				}
			} else {
				verr, ok := err.(*ValidationError)
				if !ok {
					t.Errorf("expected *ValidationError, got %T", err)
					return
				}
				if len(verr.Errors) != tt.wantErrors {
					t.Errorf(
						"Bind() got %d errors, want %d: %+v",
						len(verr.Errors),
						tt.wantErrors,
						verr.Errors,
					)
				}
			}
		})
	}
}

func TestValidate_PointerFields(t *testing.T) {
	type Form struct {
		Name *string `validate:"required"`
		Age  *int    `validate:"required"`
	}

	tests := []struct {
		name    string
		input   Form
		wantErr bool
	}{
		{name: "nil pointer", input: Form{}, wantErr: true},
		{
			name: "valid pointers",
			input: Form{
				Name: ptrStr("Alice"),
				Age:  ptrInt(30),
			},
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := tt.input
			err := Bind(&f)

			if (err != nil) != tt.wantErr {
				t.Errorf("Bind() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

// ─── Cache test ───────────────────────────────────────────────────────────

func TestValidate_CachesPerType(t *testing.T) {
	type User struct {
		Name string `validate:"required"`
	}

	// First bind: should compile and cache.
	var u1 User
	u1.Name = "Alice"
	if err := Bind(&u1); err != nil {
		t.Fatalf("first Bind() error = %v", err)
	}

	// Check that the plan is cached.
	if cached, ok := planCache.Load(reflect.TypeOf(User{})); !ok {
		t.Error("plan not cached after first Bind()")
	} else {
		if _, ok := cached.(map[string]*fieldPlan); !ok {
			t.Errorf("cached value is not map[string]*fieldPlan: %T", cached)
		}
	}

	// Second bind: should use the cached plan (no new compilation).
	var u2 User
	u2.Name = "Bob"
	if err := Bind(&u2); err != nil {
		t.Fatalf("second Bind() error = %v", err)
	}
}

// ─── Helpers ──────────────────────────────────────────────────────────────

func ptrStr(s string) *string {
	return &s
}

func ptrInt(i int) *int {
	return &i
}

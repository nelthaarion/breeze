package scalar

// diag.go — the OpenAPI registry's diagnostic probe.
//
// Scalar was the one subsystem with a *silent* failure mode: RegisterRoute
// returns immediately when Enable has not been called, so a project that wrote
// middleware.DocGET for every route and forgot scalar.Enable() gets an empty
// OpenAPI document and no error anywhere. The same is true one level up — a route
// registered without a Doc wrapper is absent from the spec, which reads as "this
// endpoint does not exist" to anything consuming the document, including an agent.
//
// This probe makes both visible: whether collection is on, how many routes were
// recorded, and how many of those recorded routes carry no title. That last
// number is the Part 5 question — a route whose description did not survive into
// the document — answered from the registry itself rather than inferred.
//
// Everything is read under the same RWMutex RegisterRoute already uses, at read
// time only. Nothing is added to registration, which happens once per route at
// startup in any case.

import (
	"fmt"
	"sort"
	"strings"

	"github.com/nelthaarion/breeze/v2/diag"
)

// diagName is the registry key. "docs" rather than "scalar", matching the
// `breeze add docs` feature name — the name a caller will have seen.
const diagName = "docs"

func init() {
	// Registered from init rather than from Enable, so that a project which
	// never called Enable is still diagnosable — and that project is precisely
	// the one whose OpenAPI document is mysteriously empty.
	//
	// This is the only init in the framework that touches the diagnostic
	// registry, and it is justified by the registry here being a package global
	// rather than a value someone constructs: there is no constructor to hook.
	diag.Register(diagName, probe)
}

// probe reports the registry's state.
func probe() diag.Report {
	mu.RLock()
	on := enabled
	count := len(routes)
	title, version, desc := apiTitle, apiVersion, apiDesc
	described, undescribed, byMethod, missing := summarise(routes)
	mu.RUnlock()

	if !on {
		return diag.Off("OpenAPI collection is off; call scalar.Enable() (or `breeze add docs`) "+
			"before registering routes").
			WithDetail("routes_recorded", count).
			WithNotes("Doc wrappers on routes are no-ops while collection is off, so the OpenAPI " +
				"document and the Scalar UI will both be empty. Nothing errors — this is the only " +
				"place it is reported.")
	}

	detail := map[string]any{
		"routes_recorded":   count,
		"with_title":        described,
		"without_title":     undescribed,
		"by_method":         byMethod,
		"api_title":         title,
		"api_version":       version,
		"has_description":   strings.TrimSpace(desc) != "",
		"undocumented_list": missing,
	}

	summary := fmt.Sprintf("%d route(s) documented, %d without a title", count, undescribed)

	var notes []string
	if count == 0 {
		notes = append(notes, "Collection is on but no route has been recorded. Routes reach this "+
			"registry through the middleware.Doc* wrappers; a route registered without one is "+
			"absent from the OpenAPI document.")
	}
	if undescribed > 0 {
		notes = append(notes, fmt.Sprintf("%d route(s) were recorded with an empty Title. They "+
			"appear in the document with their method and path but nothing describing them, which "+
			"is what a consumer reads as an undocumented endpoint.", undescribed))
	}
	if title == "" {
		notes = append(
			notes,
			"No API title is set. Call scalar.SetInfo(title, version, description) "+
				"— the document falls back to a generic name otherwise.",
		)
	}

	// An empty registry with collection on is a degraded state, not an OK one: it
	// means the wiring is half done, and the symptom is an empty document.
	if count == 0 {
		return diag.Degraded("OpenAPI collection is on but no route has been recorded", detail).
			WithNotes(notes...)
	}
	return diag.OK(summary, detail).WithNotes(notes...)
}

// summarise counts the recorded routes.
//
// Called with mu held, so it must not lock. The undocumented list is capped
// because a project with three hundred undocumented routes needs to know that,
// not to receive three hundred strings through a diagnostics endpoint.
func summarise(
	entries []routeEntry,
) (described, undescribed int, byMethod map[string]int, missing []string) {
	byMethod = map[string]int{}
	for _, e := range entries {
		byMethod[strings.ToUpper(e.method)]++
		if strings.TrimSpace(e.doc.Title) == "" {
			undescribed++
			if len(missing) < 20 {
				missing = append(missing, strings.ToUpper(e.method)+" "+e.path)
			}
			continue
		}
		described++
	}
	sort.Strings(missing)
	return described, undescribed, byMethod, missing
}

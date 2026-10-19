package worker

import "testing"

const fileParserAuditFinding = `{"id":"F1","title":"Compressed elements are inflated without an output bound","severity":"High","confidence":"high","cwe":"CWE-409","location":"tlvdoc/reader.py:42","reachability":"reachable","quality_tier":"high","trace":"parse calls read_element, which passes the payload of a type 3 element to read_compressed. That function calls zlib.decompress on the attacker-supplied bytes and holds the whole result in memory.","boundary":"Whoever supplies a document makes the service allocate memory far beyond the size of the upload.","validation":"Static trace finds no maximum output size, streaming decompressor or size cap between the element payload and zlib.decompress.","discovered_via":"source","rating":"High: a small upload exhausts process memory.","references":[{"url":"https://cwe.mitre.org/data/definitions/409.html","summary":"CWE-409 improper handling of highly compressed data","tags":"cwe"}]}`

func fileParserAuditReport(findings string) string {
	return `{"review_status":"reviewed","findings":[` + findings + `],"notes":"Static source review only.","scope":"Document reader and bundle extractor.","source_sink_inventory":["tlvdoc/reader.py:42 inflates compressed elements from the input document."],"negative_results":["tlvdoc/bundle.py:17 rejects absolute names and keeps resolved paths inside the destination."],"unverified_assumptions":["Request size and memory limits of the deploying service were not available in the checkout."],"design_properties":["tlvdoc/cli.py:14 parses the single file the user named."]}`
}

func TestFileParserAuditSchemaAndIngestion(t *testing.T) {
	report := fileParserAuditReport(fileParserAuditFinding)
	if detail := ValidateReportSchema(loadBundledSchema(t, "../../skills/audit-file-parser/schema.json"), report); detail != "" {
		t.Fatal(detail)
	}
	// The ingestion helper is shared with the build tooling audit tests.
	findings, refs := ingestBuildToolingReport(t, report)
	if len(findings) != 1 || findings[0].CWE != "CWE-409" || findings[0].Location != "tlvdoc/reader.py:42" {
		t.Fatalf("findings=%+v", findings)
	}
	if len(refs) != 1 || refs[0].URL != "https://cwe.mitre.org/data/definitions/409.html" {
		t.Fatalf("references=%+v", refs)
	}
}

func TestFileParserAuditSchemaRejectsInvalidReports(t *testing.T) {
	checkModeSchemaRejectsInvalid(t, modeSchemaCase{
		schemaPath: "../../skills/audit-file-parser/schema.json", report: fileParserAuditReport, finding: fileParserAuditFinding,
		reference:    `{"url":"https://cwe.mitre.org/data/definitions/409.html","summary":"CWE-409 improper handling of highly compressed data","tags":"cwe"}`,
		nonMatchNote: "app.py only loads its own bundled config.",
	})
}

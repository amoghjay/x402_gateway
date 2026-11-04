# Payment required

{{md .Summary}} Paid per request with x402; nothing has been charged.
{{- if .Description}}

{{md .Description}}
{{- end}}
{{- if .Message}}

{{md .Message}}
{{- end}}

## Pay from a terminal or an agent

{{range $i, $s := .Steps}}{{inc $i}}. `{{md $s.Run}}`{{if $s.Note}}  
   {{md $s.Note}}{{end}}
{{end}}
## Pay from code

`{{.SDK.Entry}}` from `{{.SDK.Import}}` (npm `{{.SDK.Package}}`): {{.SDK.Note}}.

## Terms

| | |
|---|---|
| Price | {{.Price}} {{.Asset}} ({{.Terms.Amount}} atomic units, {{.Terms.Asset}}) |
| Network | {{.Network}} (`{{.Terms.Network}}`) |
| Pay to | `{{.Terms.PayTo}}` |
| Scheme | `{{.Terms.Scheme}}`, EIP-3009 transferWithAuthorization |
| Signature valid for | up to {{.Terms.MaxTimeoutSeconds}} s |

The machine-readable terms are in the `PAYMENT-REQUIRED` response header (base64 JSON, x402 v2). Docs: {{md .Docs}}

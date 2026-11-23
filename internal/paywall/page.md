# Payment required

{{md .Summary}} Paid per request with x402; nothing has been charged.
{{- if .Description}}

{{md .Description}}
{{- end}}
{{- if .Message}}

{{md .Message}}
{{- end}}

## Pay
{{if .Commands}}
radius-cli pays it in one command: it signs the payment, retries the request and prints the response.

```sh
{{range .Commands}}{{md .}}
{{end -}}
```
{{- else}}
radius-cli pays none of the options here. Any x402 v2 client pays it: it reads the `PAYMENT-REQUIRED` header, signs one of the offered terms and retries.
{{- end}}

## Steps

{{range $i, $s := .Steps}}{{inc $i}}. `{{md $s.Run}}`{{if $s.Note}}  
   {{md $s.Note}}{{end}}
{{end}}
## Payment options

| Price | Network | Pay to | Method | Pay with |
|---|---|---|---|---|
{{range .Options}}| {{cell .PriceLabel}} | {{cell .Network}} (`{{cell .NetworkID}}`) | `{{cell .PayTo}}` | {{cell .MethodName}} | {{cell .PayWith}} |
{{end}}
## Pay from code

`{{.SDK.Entry}}` from `{{.SDK.Import}}` (npm `{{.SDK.Package}}`): {{.SDK.Note}}.

The machine-readable terms are in the `PAYMENT-REQUIRED` response header (base64 JSON, x402 v2). Docs: {{md .Docs}}

// Signs a request the way ChatGPT's agent does (Web Bot Auth: RFC 9421, Ed25519,
// Signature-Agent as a string, @authority @method @path signature-agent covered, valid
// 60 s, a nonce, tag="web-bot-auth") and prints the three headers, one per line, as
// `name: value`, ready for curl -H.
//
// usage: node wba-sign.mjs [keyfile] [agent-origin] <url> [method]
//   keyfile       the agent's Ed25519 private key as a JWK
//   agent-origin  the https origin that serves the public key at
//                 /.well-known/http-message-signatures-directory
//   WBA_KEY, WBA_AGENT   set either to leave that argument out
//   OFFSET=-200   signs as of 200 s ago (or ahead, if positive)
//
// The key is read, never printed. The keyid is the key's RFC 7638 thumbprint.
import { readFileSync } from "node:fs";
import { createPrivateKey, createHash, randomBytes, sign } from "node:crypto";

const args = process.argv.slice(2);
const keyfile = process.env.WBA_KEY || args.shift();
const agent = process.env.WBA_AGENT || args.shift();
const [url, method = "GET"] = args;
if (!keyfile || !agent || !url) {
  console.error("usage: node wba-sign.mjs [keyfile] [agent-origin] <url> [method] (or WBA_KEY, WBA_AGENT)");
  process.exit(2);
}
const jwk = JSON.parse(readFileSync(keyfile, "utf8"));
const key = createPrivateKey({ key: jwk, format: "jwk" });
const keyid = createHash("sha256").update(JSON.stringify({ crv: "Ed25519", kty: "OKP", x: jwk.x })).digest("base64url");

const u = new URL(url);
const authority = u.host.toLowerCase(); // URL drops the scheme's default port
const now = Math.floor(Date.now() / 1000) + Number(process.env.OFFSET || 0);
const params = `("@authority" "@method" "@path" "signature-agent");created=${now};expires=${now + 60}` +
  `;keyid="${keyid}";alg="ed25519";nonce="${randomBytes(32).toString("base64")}";tag="web-bot-auth"`;
const agentField = `"${agent}"`;
const base = [
  `"@authority": ${authority}`,
  `"@method": ${method.toUpperCase()}`,
  `"@path": ${u.pathname || "/"}`,
  `"signature-agent": ${agentField}`,
  `"@signature-params": ${params}`,
].join("\n");
const sig = sign(null, Buffer.from(base), key).toString("base64");

console.log(`Signature-Agent: ${agentField}`);
console.log(`Signature-Input: sig1=${params}`);
console.log(`Signature: sig1=:${sig}:`);

/* Copyable client snippets for the current model. All samples hit the
 * same-origin BFF; the browser session injects credentials, so none carry
 * an API key. */

export type SampleLang = "curl" | "javascript" | "python" | "go";

export const SAMPLE_LANGS: SampleLang[] = ["curl", "javascript", "python", "go"];

const PROMPT = "Explain the difference between liveness and readiness for a gateway region.";

export function codeSample(lang: SampleLang, origin: string, model: string): string {
  const url = `${origin}/api/llm/responses`;
  switch (lang) {
    case "curl":
      return `# Credentials are injected by the browser session / BFF.
curl -N "${url}" \\
  -H "Content-Type: application/json" \\
  -H "Accept: text/event-stream" \\
  -d '{
    "model": "${model}",
    "input": "${PROMPT}",
    "store": false,
    "stream": true
  }'`;
    case "javascript":
      return `// Credentials are injected by the browser session / BFF.
const resp = await fetch("${url}", {
  method: "POST",
  headers: {
    "Content-Type": "application/json",
    Accept: "text/event-stream",
  },
  body: JSON.stringify({
    model: "${model}",
    input: "${PROMPT}",
    store: false,
    stream: true,
  }),
});
// Read named SSE frames until response.completed — there is no [DONE].`;
    case "python":
      return `import requests

# Credentials are injected by the browser session / BFF.
resp = requests.post(
    "${url}",
    json={
        "model": "${model}",
        "input": "${PROMPT}",
        "store": False,
        "stream": True,
    },
    headers={"Accept": "text/event-stream"},
    stream=True,
)
# Iterate named SSE frames until response.completed — there is no [DONE].`;
    case "go":
      return `// Credentials are injected by the browser session / BFF.
body := \`{"model":"${model}","input":"${PROMPT}","store":false,"stream":true}\`
req, _ := http.NewRequest("POST", "${url}", strings.NewReader(body))
req.Header.Set("Content-Type", "application/json")
req.Header.Set("Accept", "text/event-stream")
resp, err := http.DefaultClient.Do(req)
// Scan named SSE frames until response.completed — there is no [DONE].`;
  }
}

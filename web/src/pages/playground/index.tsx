import { useEffect, useMemo, useState } from "react";
import { useApi } from "../../api/useApi";
import { ErrorBanner, Loading } from "../../components/feedback";
import { EmptyState } from "../../components/ui";
import { ChatPanel } from "./ChatPanel";
import { InspectorPanel } from "./InspectorPanel";
import type { ModelList } from "./types";
import { useResponsesStream } from "./useResponsesStream";

/**
 * Playground — the fully-integrated console page. Real models from
 * GET /llm/models, real single-turn Responses calls (streamed via SSE or not),
 * ephemeral client-side history, and a request inspector. Full-viewport split
 * layout: chat on the left, inspector on the right.
 */
export default function PlaygroundPage() {
  const { data, error, loading, retry } = useApi<ModelList>("/llm/models");
  const models = useMemo(
    () => (data?.data ?? []).map((m) => ({ id: m.id, ownedBy: m.owned_by })),
    [data],
  );

  const [model, setModel] = useState("");
  const [stream, setStream] = useState(true);
  const [input, setInput] = useState("");
  const run = useResponsesStream();

  // Default to the first advertised model; never invent a fallback.
  useEffect(() => {
    if (!model && models.length > 0) setModel(models[0].id);
  }, [models, model]);

  const modelMissing = model !== "" && !models.some((m) => m.id === model);

  const send = (text?: string) => {
    const value = (text ?? input).trim();
    if (!value || run.status === "streaming" || !model || modelMissing) return;
    setInput("");
    void run.send(model, value, stream);
  };

  if (loading) return <Loading />;
  if (error) return <ErrorBanner error={error} retry={retry} />;
  if (models.length === 0) {
    return (
      <EmptyState
        title="No models available"
        hint="GET /llm/models returned an empty list — publish a routing catalog with at least one route."
      />
    );
  }

  const lastAssistant = [...run.messages].reverse().find((m) => m.role === "assistant");

  return (
    <div
      style={{
        height: "calc(100vh - var(--header-h) - 40px)",
        display: "flex",
        gap: 16,
        minHeight: 0,
      }}
    >
      <ChatPanel
        model={model}
        modelMissing={modelMissing}
        messages={run.messages}
        status={run.status}
        input={input}
        onInput={setInput}
        onSend={send}
        onStop={run.stop}
        onNewChat={run.reset}
      />
      <InspectorPanel
        models={models}
        model={model}
        onModelChange={setModel}
        modelMissing={modelMissing}
        stream={stream}
        onStreamChange={setStream}
        status={run.status}
        assistant={lastAssistant}
        events={run.events}
        finalResponse={run.finalResponse}
        requestBody={run.requestBody}
        error={run.error}
        durationMs={run.durationMs}
      />
    </div>
  );
}

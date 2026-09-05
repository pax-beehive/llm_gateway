import { useState } from "react";
import { Modal, ErrorBanner, Loading } from "../../components/feedback";
import { Button } from "../../components/ui";
import { getDraft, listDrafts } from "../routing/api";
import type { Draft } from "../routing/types";
import { hasValidationReport } from "../routing/types";
import { useResource } from "../overview/lib";
import { ApiError } from "../../api/client";

export function SavedModelDrafts({ onClose, onOpen }: { onClose: () => void; onOpen: (draft: Draft) => void }) {
  const [cursor, setCursor] = useState("");
  const resource = useResource(() => listDrafts(cursor, 25), [cursor]);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<ApiError | null>(null);
  const open = async (id: string) => {
    setBusy(true); setError(null);
    try { onOpen(await getDraft(id)); }
    catch (err) { setError(err instanceof ApiError ? err : new ApiError(0, "request_failed", "Could not open draft.")); }
    finally { setBusy(false); }
  };
  return <Modal open title="Continue saved draft" onClose={() => { if (!busy) onClose(); }}>
    {resource.loading && <Loading />}
    {(error || resource.error) && <ErrorBanner error={(error || resource.error)!} />}
    <div style={{ display: "grid", gap: 12 }}>
      {resource.data?.data.map(draft => <div key={draft.id} style={{ borderBottom: "1px solid var(--line)", paddingBottom: 12 }}>
        <div style={{ overflowWrap: "anywhere" }}>{draft.id}</div>
        <div style={{ fontSize: 12, margin: "5px 0" }}>{hasValidationReport(draft) ? draft.validation_report.valid ? "Ready to publish" : `${draft.validation_report.errors?.length ?? 0} issues to resolve` : "Needs validation"} · {draft.document.routes.length} routes</div>
        <Button disabled={busy} onClick={() => void open(draft.id)}>Continue setup</Button>
      </div>)}
      {!resource.loading && resource.data?.data.length === 0 && <p>No saved drafts.</p>}
      {resource.data?.next_cursor && <Button onClick={() => setCursor(resource.data!.next_cursor!)}>Older drafts</Button>}
      {cursor && <Button onClick={() => setCursor("")}>Latest drafts</Button>}
    </div>
  </Modal>;
}

// reconcile.ts — pure helper for handleTabsList lazy reconcile (Drift-G fallback).
// Does NOT depend on chrome.* APIs so it can be unit-tested with vitest.
// computeReconcileTargets returns ids of tabs that are NOT in the controlled set
// but whose openerTabId IS in the controlled set (i.e. window.open'd from a
// controlled tab) — these should be folded into the MCP group.

export interface ReconcileTab {
  id: number;
  openerTabId?: number;
}

export function computeReconcileTargets(
  tabs: ReconcileTab[],
  controlledIds: Set<number>,
): number[] {
  const result: number[] = [];
  for (const t of tabs) {
    if (controlledIds.has(t.id)) continue; // already controlled
    if (t.openerTabId !== undefined && controlledIds.has(t.openerTabId)) {
      result.push(t.id);
    }
  }
  return result;
}

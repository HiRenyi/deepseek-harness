import { describe, it, expect } from "vitest";
import { computeReconcileTargets } from "./reconcile";

describe("computeReconcileTargets", () => {
  it("returns tabs whose opener is controlled but self not in set", () => {
    const tabs = [
      { id: 1, openerTabId: undefined },        // controlled (root)
      { id: 2, openerTabId: 1 },                 // window.open from 1 → reconcile
      { id: 3, openerTabId: 2 },                 // opener 2 not yet controlled → skip
      { id: 4, openerTabId: 999 },               // opener not controlled → skip
    ];
    const controlled = new Set<number>([1]);
    expect(computeReconcileTargets(tabs, controlled)).toEqual([2]);
  });

  it("skips tabs already in controlled set", () => {
    const tabs = [{ id: 1, openerTabId: 1 }];
    const controlled = new Set<number>([1]);
    expect(computeReconcileTargets(tabs, controlled)).toEqual([]);
  });

  it("returns empty for empty controlled set (no attachedTabId, no group)", () => {
    const tabs = [{ id: 1, openerTabId: 2 }];
    expect(computeReconcileTargets(tabs, new Set<number>())).toEqual([]);
  });

  it("handles tabs with no openerTabId (Ctrl+T, user-initiated)", () => {
    const tabs = [{ id: 5 }]; // no openerTabId
    const controlled = new Set<number>([1]);
    expect(computeReconcileTargets(tabs, controlled)).toEqual([]);
  });
});

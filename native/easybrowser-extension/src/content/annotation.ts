/**
 * Browser MCP — Element Annotation Content Script
 *
 * Hybrid annotation:
 *   Phase 1 (instant): CSS selector-based overlay labels on interactive elements
 *   Phase 2 (async): Request snapshot from background, upgrade labels with nodeId
 *
 * Injected/removed by background.ts via chrome.scripting.executeScript.
 * All created elements are marked with data-mcp-annotation for cleanup.
 */

const INTERACTIVE_SELECTORS = [
  'a[href]',
  'button',
  'input',
  'select',
  'textarea',
  '[role="button"]',
  '[role="link"]',
  '[role="textbox"]',
  '[role="combobox"]',
  '[role="checkbox"]',
  '[role="radio"]',
  '[tabindex]:not([tabindex="-1"])',
  '[contenteditable="true"]',
];

const ANNOTATION_ZINDEX = 2147483646; // Just below max

interface AnnotationOverlay {
  element: HTMLElement;
  overlay: HTMLElement;
  label: string;
  nodeId?: number;
}

const overlays: AnnotationOverlay[] = [];

function getLabel(el: HTMLElement): string {
  const tag = el.tagName.toLowerCase();
  const role = el.getAttribute('role');
  const type = el.getAttribute('type');
  const name = el.getAttribute('name') || el.getAttribute('placeholder') || '';
  const text = (el.textContent || '').trim().slice(0, 20);

  let label = tag;
  if (type) label += `[${type}]`;
  else if (role) label += `[${role}]`;
  if (text) label += `: ${text}`;
  else if (name) label += `: ${name}`;

  return label;
}

function createOverlay(el: HTMLElement): HTMLElement {
  const rect = el.getBoundingClientRect();
  const overlay = document.createElement('div');
  overlay.setAttribute('data-mcp-annotation', 'overlay');

  const label = getLabel(el);

  overlay.style.cssText = `
    position: fixed;
    left: ${rect.left}px;
    top: ${rect.top - 20}px;
    z-index: ${ANNOTATION_ZINDEX};
    padding: 1px 5px;
    background: rgba(74, 144, 217, 0.9);
    color: white;
    border-radius: 3px;
    font-family: system-ui, sans-serif;
    font-size: 10px;
    line-height: 14px;
    white-space: nowrap;
    pointer-events: none;
    max-width: 200px;
    overflow: hidden;
    text-overflow: ellipsis;
  `;

  overlay.textContent = label;
  overlay.dataset.label = label;
  overlay.dataset.selector = getCssSelector(el);

  document.body.appendChild(overlay);

  overlays.push({ element: el, overlay, label });

  return overlay;
}

function getCssSelector(el: HTMLElement): string {
  if (el.id) return `#${el.id}`;
  const tag = el.tagName.toLowerCase();
  const parent = el.parentElement;
  if (!parent) return tag;
  const siblings = Array.from(parent.children).filter(c => c.tagName === el.tagName);
  if (siblings.length === 1) return `${getCssSelector(parent)} > ${tag}`;
  const idx = siblings.indexOf(el) + 1;
  return `${getCssSelector(parent)} > ${tag}:nth-of-type(${idx})`;
}

function phase1_cssAnnotation(): void {
  const selector = INTERACTIVE_SELECTORS.join(', ');
  const elements = document.querySelectorAll(selector);

  elements.forEach((el) => {
    const htmlEl = el as HTMLElement;

    // Skip invisible elements
    if (htmlEl.offsetParent === null && htmlEl.style.position !== 'fixed') return;
    const rect = htmlEl.getBoundingClientRect();
    if (rect.width === 0 || rect.height === 0) return;

    // Skip elements outside viewport
    if (rect.top < -100 || rect.left < -100 || rect.top > window.innerHeight + 100) return;

    // Add highlight border
    htmlEl.setAttribute('data-mcp-annotation', 'highlight');
    htmlEl.style.cssText += 'outline: 1px dashed rgba(74, 144, 217, 0.6) !important; outline-offset: 1px !important;';

    createOverlay(htmlEl);
  });

  // Add indicator
  const indicator = document.createElement('div');
  indicator.setAttribute('data-mcp-annotation', 'indicator');
  indicator.style.cssText = `
    position: fixed;
    top: 8px;
    right: 8px;
    z-index: 2147483647;
    padding: 6px 12px;
    background: #4A90D9;
    color: white;
    border-radius: 4px;
    font-family: system-ui, sans-serif;
    font-size: 12px;
    font-weight: 500;
    box-shadow: 0 2px 8px rgba(0,0,0,0.2);
    pointer-events: none;
  `;
  indicator.textContent = `🏷️ 标注 (${overlays.length} 元素)`;
  document.body.appendChild(indicator);

  // Phase 2: async snapshot upgrade
  phase2_snapshotUpgrade();
}

async function phase2_snapshotUpgrade(): Promise<void> {
  // Request snapshot from background → bridge
  try {
    const response = await chrome.runtime.sendMessage({
      type: 'GET_SNAPSHOT_ANNOTATIONS'
    });

    if (!response?.annotations) return;

    // Upgrade overlays with nodeId data
    for (const overlay of overlays) {
      const selector = overlay.overlay.dataset.selector;
      const annotation = response.annotations.find((a: { selector: string; nodeId: number }) => a.selector === selector);
      if (annotation) {
        overlay.nodeId = annotation.nodeId;
        overlay.overlay.textContent = `${overlay.label} [node:${annotation.nodeId}]`;
      }
    }

    // Update indicator
    const indicator = document.querySelector('[data-mcp-annotation="indicator"]');
    if (indicator) {
      indicator.textContent = `🏷️ 标注 (${overlays.length} 元素, nodeId ✓)`;
    }
  } catch (e) {
    // Snapshot upgrade is optional — CSS annotations are still useful
    console.warn('[BrowserMCP] Snapshot upgrade failed:', e);
  }
}

// Run Phase 1 immediately
phase1_cssAnnotation();

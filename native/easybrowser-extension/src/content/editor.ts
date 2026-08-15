/**
 * Browser MCP — Page Editor Content Script
 *
 * Makes interactive elements editable and adds visual highlights.
 * Injected/removed by background.ts via chrome.scripting.executeScript.
 * All created elements are marked with data-mcp-editor for cleanup.
 */

const HIGHLIGHT_STYLE = `
  outline: 2px solid #4A90D9 !important;
  outline-offset: 2px !important;
  background-color: rgba(74, 144, 217, 0.08) !important;
`;

const INTERACTIVE_SELECTORS = [
  'input[type="text"]',
  'input[type="search"]',
  'input[type="email"]',
  'input[type="url"]',
  'input[type="tel"]',
  'input[type="number"]',
  'input[type="password"]',
  'input:not([type])',
  'textarea',
  '[contenteditable="true"]',
  '[role="textbox"]',
];

function initEditor(): void {
  // Find all editable text elements
  const selector = INTERACTIVE_SELECTORS.join(', ');
  const elements = document.querySelectorAll(selector);

  elements.forEach((el) => {
    const htmlEl = el as HTMLElement;

    // Skip already editable elements
    if (htmlEl.isContentEditable && !htmlEl.hasAttribute('data-mcp-editable')) return;

    // Mark for cleanup
    htmlEl.setAttribute('data-mcp-editable', 'true');

    // Make editable
    if (!htmlEl.isContentEditable) {
      htmlEl.setAttribute('contenteditable', 'true');
    }

    // Add highlight
    htmlEl.setAttribute('data-mcp-editor', 'highlight');
    htmlEl.style.cssText += HIGHLIGHT_STYLE;
  });

  // Add a floating indicator
  const indicator = document.createElement('div');
  indicator.setAttribute('data-mcp-editor', 'indicator');
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
  indicator.textContent = '🖊️ 编辑模式';
  document.body.appendChild(indicator);
}

// Run immediately
initEditor();

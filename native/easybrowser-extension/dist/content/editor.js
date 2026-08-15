"use strict";
(() => {
  // src/content/editor.ts
  var HIGHLIGHT_STYLE = `
  outline: 2px solid #4A90D9 !important;
  outline-offset: 2px !important;
  background-color: rgba(74, 144, 217, 0.08) !important;
`;
  var INTERACTIVE_SELECTORS = [
    'input[type="text"]',
    'input[type="search"]',
    'input[type="email"]',
    'input[type="url"]',
    'input[type="tel"]',
    'input[type="number"]',
    'input[type="password"]',
    "input:not([type])",
    "textarea",
    '[contenteditable="true"]',
    '[role="textbox"]'
  ];
  function initEditor() {
    const selector = INTERACTIVE_SELECTORS.join(", ");
    const elements = document.querySelectorAll(selector);
    elements.forEach((el) => {
      const htmlEl = el;
      if (htmlEl.isContentEditable && !htmlEl.hasAttribute("data-mcp-editable")) return;
      htmlEl.setAttribute("data-mcp-editable", "true");
      if (!htmlEl.isContentEditable) {
        htmlEl.setAttribute("contenteditable", "true");
      }
      htmlEl.setAttribute("data-mcp-editor", "highlight");
      htmlEl.style.cssText += HIGHLIGHT_STYLE;
    });
    const indicator = document.createElement("div");
    indicator.setAttribute("data-mcp-editor", "indicator");
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
    indicator.textContent = "🖊️ 编辑模式";
    document.body.appendChild(indicator);
  }
  initEditor();
})();

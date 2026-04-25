(function () {
  function detectTheme() {
    var html = document.documentElement;
    var theme = html.classList.contains("light")
      ? "default"
      : "dark";
    return theme;
  }

  function convertCodeBlocks() {
    var blocks = document.querySelectorAll("pre code.language-mermaid");
    blocks.forEach(function (code) {
      var pre = code.parentElement;
      var wrapper = document.createElement("pre");
      wrapper.className = "mermaid";
      wrapper.textContent = code.textContent;
      pre.parentElement.replaceChild(wrapper, pre);
    });
  }

  function init() {
    if (typeof mermaid === "undefined") return;
    convertCodeBlocks();
    mermaid.initialize({
      startOnLoad: false,
      theme: detectTheme(),
      securityLevel: "loose",
      flowchart: { htmlLabels: true, curve: "basis" },
      sequence: { useMaxWidth: true, mirrorActors: false },
      themeVariables: {
        fontFamily:
          "ui-sans-serif, system-ui, -apple-system, BlinkMacSystemFont, sans-serif",
      },
    });
    mermaid.run({ querySelector: ".mermaid" });
  }

  if (document.readyState === "loading") {
    document.addEventListener("DOMContentLoaded", init);
  } else {
    init();
  }

  var themeButtons = document.querySelectorAll(".theme");
  themeButtons.forEach(function (btn) {
    btn.addEventListener("click", function () {
      setTimeout(function () {
        document.querySelectorAll(".mermaid").forEach(function (el) {
          if (el.dataset.processed === "true") {
            el.removeAttribute("data-processed");
            el.innerHTML = el.dataset.originalSource || el.textContent;
          }
        });
        mermaid.initialize({
          startOnLoad: false,
          theme: detectTheme(),
          securityLevel: "loose",
        });
        mermaid.run({ querySelector: ".mermaid" });
      }, 50);
    });
  });
})();

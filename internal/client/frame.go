package client

import (
	"fmt"
	"net/http"
)

// reportPolicy keeps an interactive report off the network and away from the
// page: scripts and styles only inline, images and fonts only as data. The
// page (codeflower-ui's report.ts) puts the same policy in the report itself.
const reportPolicy = "default-src 'none'; script-src 'unsafe-inline'; style-src 'unsafe-inline'; img-src data: blob:; font-src data:; media-src data: blob:; worker-src blob:"

// frameDocument is the empty document an interactive report is written into.
// The page loads it in an iframe sandboxed without allow-same-origin, so the
// report has no cookie and cannot reach the page or the API; the frame asks
// the page for the report, and writes it in under this document's policy.
const frameDocument = `<!doctype html><meta charset="utf-8"><script>
addEventListener('message', function load(event) {
  var message = event.data;
  if (event.source !== parent || !message || message.type !== 'cf-load' || typeof message.html !== 'string') return;
  removeEventListener('message', load);
  document.open();
  document.write(message.html);
  document.close();
});
parent.postMessage({ type: 'cf-frame-ready' }, '*');
</script>`

// serveFrame needs no cookie: the sandboxed frame has none, and the document
// holds nothing. Only this app's page may frame it.
func serveFrame(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Security-Policy", reportPolicy+"; frame-ancestors 'self'")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	fmt.Fprint(w, frameDocument)
}

package companion

// Sidebar navigation keeps the shared shell mounted while replacing the
// server-rendered page. Ordinary links and form submissions remain native.
const navigationScript = `<script>
/* companion-navigation */
(() => {
  const routes = new Set(['/keys', '/dashboard', '/admin', '/audit', '/prices', '/logs']);
  const nonce = document.currentScript.nonce;
  let pending;
  let sequence = 0;

  function settleAvatar() {
    document.querySelectorAll('.avatar img').forEach(img => {
      if (img.complete && img.naturalWidth === 0) img.remove();
      else img.addEventListener('error', () => img.remove(), {once: true});
    });
  }
  settleAvatar();

  async function navigate(destination, push) {
    const url = new URL(destination, location.href);
    if (url.origin !== location.origin || !routes.has(url.pathname)) return;
    const current = ++sequence;
    pending?.abort();
    const controller = new AbortController();
    pending = controller;
    try {
      const response = await fetch(url.href, {
        credentials: 'same-origin',
        headers: {Accept: 'text/html'},
        signal: controller.signal
      });
      if (response.redirected || !response.ok || !(response.headers.get('content-type') || '').includes('text/html')) {
        location.assign(url.href);
        return;
      }
      const next = new DOMParser().parseFromString(await response.text(), 'text/html');
      if (current !== sequence) return;
      const shell = document.querySelector('.shell');
      const sidebar = shell?.querySelector('.sidebar');
      const main = shell?.querySelector('main.content');
      const nextShell = next.querySelector('.shell');
      const nextSidebar = nextShell?.querySelector('.sidebar');
      const nextMain = nextShell?.querySelector('main.content');
      if (!sidebar || !main || !nextSidebar || !nextMain) {
        location.assign(url.href);
        return;
      }

      const oldStyles = [...document.head.querySelectorAll('style')].slice(1);
      const newStyles = [...next.head.querySelectorAll('style')].slice(1);
      newStyles.forEach(style => document.head.append(style.cloneNode(true)));
      oldStyles.forEach(style => style.remove());
      window.__companionSelectAbort?.abort();
      sidebar.replaceChildren(...nextSidebar.childNodes);
      settleAvatar();
      main.replaceWith(nextMain);
      document.querySelectorAll('body > :not(.shell)').forEach(node => node.remove());
      [...next.body.children].filter(node => node !== nextShell && node.tagName !== 'SCRIPT')
        .forEach(node => document.body.append(node));
      document.title = next.title;
      if (push) history.pushState({companion: true}, '', url.href);
      window.scrollTo(0, 0);

      next.body.querySelectorAll('script').forEach(source => {
        if (source.textContent.includes('/* companion-navigation */')) return;
        const script = document.createElement('script');
        script.nonce = nonce;
        script.textContent = source.textContent;
        document.body.append(script);
        script.remove();
      });
    } catch (error) {
      if (error.name !== 'AbortError' && current === sequence) location.assign(url.href);
    } finally {
      if (pending === controller) pending = undefined;
    }
  }

  document.addEventListener('click', event => {
    const link = event.target.closest('.sidebar .nav a[href]');
    if (!link || event.defaultPrevented || event.button !== 0 || event.metaKey || event.ctrlKey ||
        event.shiftKey || event.altKey || link.target || link.download) return;
    const url = new URL(link.href);
    if (url.origin !== location.origin || !routes.has(url.pathname)) return;
    event.preventDefault();
    if (url.href !== location.href) navigate(url.href, true);
  });
  window.addEventListener('popstate', () => navigate(location.href, false));
})();
</script>`

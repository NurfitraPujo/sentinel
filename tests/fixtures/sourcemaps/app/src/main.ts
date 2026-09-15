// Entry point. Every feature is a lazy route so Rollup emits one chunk per feature plus the shared
// lib chunk, which is what makes the fixture's maps plural (contract §13: "10 frames each across 5
// of the built chunks").
type Route = 'cart' | 'checkout' | 'search' | 'auth' | 'analytics';

const root = document.getElementById('app') as HTMLElement;

async function mount(route: Route): Promise<void> {
  switch (route) {
    case 'cart': {
      const cart = await import('./cart');
      cart.addToCart({ sku: 'sku-1', quantity: 1, unitPrice: { amount: 1250, currency: 'USD' } });
      cart.renderCart(root);
      return;
    }
    case 'checkout': {
      const checkout = await import('./checkout');
      const total = { amount: 1250, currency: 'USD' };
      checkout.renderSummary(root, total, checkout.shippingFor('US', total));
      return;
    }
    case 'search': {
      const search = await import('./search');
      const hits = await search.runSearch(location.search.slice(1) || 'shoes');
      root.textContent = hits.map((h) => search.highlight(h.title, ['shoes'])).join(', ');
      return;
    }
    case 'auth': {
      const auth = await import('./auth');
      await auth.login('user@example.com', 'hunter2');
      root.textContent = auth.requireSession().token;
      return;
    }
    case 'analytics': {
      const analytics = await import('./analytics');
      analytics.pageView(location.pathname);
      root.textContent = `${analytics.pending()} pending`;
      await analytics.flush();
      return;
    }
  }
}

function routeFromHash(): Route {
  const hash = location.hash.replace(/^#\/?/, '');
  const known: Route[] = ['cart', 'checkout', 'search', 'auth', 'analytics'];
  const found = known.find((r) => r === hash);
  if (!found) {
    throw new Error(`routeFromHash: unknown route ${hash}`);
  }
  return found;
}

function boot(): void {
  window.addEventListener('hashchange', () => void mount(routeFromHash()));
  void mount(routeFromHash());
}

boot();

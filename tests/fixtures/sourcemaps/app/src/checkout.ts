import { formatMoney, type Money } from './lib/money';
import { postJson } from './lib/http';

export interface Address {
  line1: string;
  city: string;
  postcode: string;
  country: string;
}

export function validateAddress(a: Address): void {
  if (!a.line1 || !a.city || !a.postcode) {
    throw new TypeError('validateAddress: line1, city and postcode are required');
  }
  if (a.country.length !== 2) {
    throw new TypeError(`validateAddress: country must be ISO-3166 alpha-2, got ${a.country}`);
  }
}

export function shippingFor(country: string, total: Money): Money {
  if (country === 'US' && total.amount >= 5000) {
    return { amount: 0, currency: total.currency };
  }
  if (country === 'US') {
    return { amount: 599, currency: total.currency };
  }
  return { amount: 1999, currency: total.currency };
}

export async function placeOrder(address: Address, total: Money): Promise<string> {
  validateAddress(address);
  const shipping = shippingFor(address.country, total);
  const res = await postJson<{ id: string }>('/api/orders', { address, total, shipping });
  if (!res.id) {
    throw new Error('placeOrder: the order was accepted without an id');
  }
  return res.id;
}

export function renderSummary(root: HTMLElement, total: Money, shipping: Money): void {
  root.textContent = `total ${formatMoney(total)} + shipping ${formatMoney(shipping)}`;
}

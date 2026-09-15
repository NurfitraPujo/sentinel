import { addMoney, applyPercent, formatMoney, type Money } from './lib/money';
import { postJson } from './lib/http';

export interface CartLine {
  sku: string;
  quantity: number;
  unitPrice: Money;
}

const lines: CartLine[] = [];

export function addToCart(line: CartLine): number {
  if (line.quantity <= 0) {
    throw new RangeError(`addToCart: quantity must be positive, got ${line.quantity}`);
  }
  const existing = lines.find((l) => l.sku === line.sku);
  if (existing) {
    existing.quantity += line.quantity;
  } else {
    lines.push({ ...line });
  }
  return lines.length;
}

export function removeFromCart(sku: string): void {
  const index = lines.findIndex((l) => l.sku === sku);
  if (index < 0) {
    throw new Error(`removeFromCart: ${sku} is not in the cart`);
  }
  lines.splice(index, 1);
}

export function subtotal(): Money {
  return lines.reduce<Money>(
    (acc, l) => addMoney(acc, { amount: l.unitPrice.amount * l.quantity, currency: l.unitPrice.currency }),
    { amount: 0, currency: 'USD' },
  );
}

export function applyCoupon(code: string): Money {
  const percent = code === 'TEN' ? 10 : code === 'HALF' ? 50 : Number.NaN;
  if (Number.isNaN(percent)) {
    throw new Error(`applyCoupon: unknown coupon ${code}`);
  }
  return applyPercent(subtotal(), percent);
}

export async function saveCart(): Promise<void> {
  await postJson('/api/cart', { lines });
}

export function renderCart(root: HTMLElement): void {
  root.textContent = `${lines.length} lines, ${formatMoney(subtotal())}`;
}

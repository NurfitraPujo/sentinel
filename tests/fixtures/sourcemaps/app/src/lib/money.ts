// Shared helpers imported by more than one lazy route, so Rollup lifts them into a shared chunk.
export interface Money {
  amount: number;
  currency: string;
}

export function formatMoney(m: Money): string {
  if (!Number.isFinite(m.amount)) {
    throw new RangeError(`formatMoney: amount is not finite (${m.amount})`);
  }
  return `${m.currency} ${(m.amount / 100).toFixed(2)}`;
}

export function addMoney(a: Money, b: Money): Money {
  if (a.currency !== b.currency) {
    throw new TypeError(`addMoney: currency mismatch ${a.currency} vs ${b.currency}`);
  }
  return { amount: a.amount + b.amount, currency: a.currency };
}

export function applyPercent(m: Money, percent: number): Money {
  if (percent < 0 || percent > 100) {
    throw new RangeError(`applyPercent: ${percent} is outside 0..100`);
  }
  return { amount: Math.round((m.amount * (100 - percent)) / 100), currency: m.currency };
}

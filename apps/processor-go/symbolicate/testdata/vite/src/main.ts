export interface Cart {
  items: number[];
  coupon?: string;
}

export function subtotal(cart: Cart): number {
  let total = 0;
  for (const price of cart.items) {
    total += price;
  }
  return total;
}

export function applyCoupon(cart: Cart, total: number): number {
  if (cart.coupon === undefined) {
    return total;
  }
  if (cart.coupon !== "SAVE10") {
    throw new Error("unknown coupon " + cart.coupon);
  }
  return total * 0.9;
}

export class Checkout {
  constructor(private readonly cart: Cart) {}

  pay(): number {
    const base = subtotal(this.cart);
    const due = applyCoupon(this.cart, base);
    if (due < 0) {
      throw new RangeError("negative total");
    }
    return due;
  }
}

const checkout = new Checkout({ items: [10, 20, 30], coupon: "BOGUS" });
console.log(checkout.pay());

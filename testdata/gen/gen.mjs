// Generates golden expectations from the TypeScript implementation (@ecom/contracts) so the Go port can be checked
// against it: pricing, coupons, shipping, money formatting, serviceability, summaries, sorting and permissions.
//   node testdata/gen/gen.mjs        (needs ../eCommerce-contracts built: pnpm build there)
import { readFileSync, writeFileSync } from 'node:fs';
import { dirname, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';
import * as c from '../../../eCommerce-contracts/dist/index.js';

const here = dirname(fileURLToPath(import.meta.url));
const products = JSON.parse(readFileSync(resolve(here, '../products.json'), 'utf8'));
const NOW = Date.parse('2026-03-01T10:00:00.000Z');

// A small deterministic RNG so the fixtures are reproducible.
let seed = 20260301;
const rnd = () => ((seed = (seed * 1664525 + 1013904223) >>> 0) / 2 ** 32);
const pick = (a) => a[Math.floor(rnd() * a.length)];
const int = (lo, hi) => lo + Math.floor(rnd() * (hi - lo + 1));

const variants = products.flatMap((p) => p.variants.map((v) => ({ p, v })));
const COUPONS = ['WELCOME10', 'FLAT100', 'FREESHIP', 'EXPIRED50', 'nope', ' welcome10 '];

const cartCases = [];
for (let i = 0; i < 400; i++) {
  const overrides = {};
  const items = [];
  const n = int(1, 5);
  for (let k = 0; k < n; k++) {
    const { v } = pick(variants);
    if (items.some((x) => x.variantId === v.id)) continue;
    const roll = rnd();
    if (roll < 0.15) overrides[v.id] = { stock: 0 };
    else if (roll < 0.3) overrides[v.id] = { stock: int(1, 4) };
    else if (roll < 0.36) overrides[v.id] = { stock: 0, backorder: { expectedDate: '2026-04-01T00:00:00.000Z' } };
    const seen = rnd() < 0.8 ? v.price.amount : Math.max(100, v.price.amount + int(-5000, 5000));
    items.push({ variantId: v.id, quantity: int(1, 12), seenPrice: seen });
  }
  if (rnd() < 0.05) items.push({ variantId: 'no-such-variant', quantity: 1, seenPrice: 100 });
  const stored = { items, shippingMethod: pick(['standard', 'express']) };
  if (rnd() < 0.6) stored.couponCode = pick(COUPONS);
  const patched = products.map((p) => ({ ...p, variants: p.variants.map((v) => (overrides[v.id] ? { ...v, ...overrides[v.id] } : v)) }));
  cartCases.push({ stored, overrides, expected: c.priceCart(stored, patched, NOW) });
}

const coupons = [];
for (const code of COUPONS) for (const subtotal of [0, 49_900, 69_900, 99_899, 99_900, 150_000, 999_999]) coupons.push({ code, subtotal, expected: c.evaluateCoupon(code, subtotal, NOW) });

const shipping = [];
for (const method of ['standard', 'express']) for (const amt of [-1, 0, 4_999, 49_899, 49_900, 120_000]) for (const free of [false, true]) shipping.push({ method, amt, free, expected: c.shippingFee(method, amt, free) });

const money = [0, 1, 99, 100, 150, 4_900, 12_345, 99_900, 129_900, 1_234_567, 12_345_678, 123_456_789, 1_00_00_000_00].map((amount) => ({ amount, expected: c.formatMoney({ amount, currency: 'INR' }) }));

const pincodes = ['110001', '400001', '700001', '800001', '900001', '560001', '123456', '234567', '345678', '456789', '567890', '678901', '789012', '890123'].map((pincode) => ({ pincode, expected: c.computeServiceability(pincode, NOW) }));

const cod = [];
for (const pincode of ['110001', '700001', '800001', '560001']) for (const total of [0, 499_999, 500_000, 500_001]) cod.push({ pincode, total, expected: c.codEligibility(pincode, total) });

const passwords = ['', 'short', 'alllowercase1', 'ALLUPPERCASE1', 'NoDigitsHere', 'Str0ngPass', 'Abcdefg1', 'ünïcödé1Aa'].map((password) => ({ password, expected: c.passwordProblem(password) }));

const permissions = [['customer'], ['admin'], ['customer', 'admin'], ['seller'], ['admin', 'seller']].map((roles) => ({ roles, expected: c.permissionsFor(roles) }));

const summaries = products.map((p) => ({ id: p.id, expected: c.toSummary(p) }));
const sorts = ['relevance', 'featured', 'price-asc', 'price-desc', 'newest', 'rating', 'discount'].map((sort) => ({ sort, expected: c.sortProducts(products, sort).map((p) => p.id) }));
const stock = products.map((p) => ({ id: p.id, expected: c.stockStatusOf(p), discount: c.bestDiscount(p) }));

writeFileSync(resolve(here, '../golden.json'), JSON.stringify({ now: '2026-03-01T10:00:00.000Z', cartCases, coupons, shipping, money, pincodes, cod, passwords, permissions, summaries, sorts, stock }));
console.log(`golden.json: ${cartCases.length} carts, ${coupons.length} coupons, ${summaries.length} summaries`);

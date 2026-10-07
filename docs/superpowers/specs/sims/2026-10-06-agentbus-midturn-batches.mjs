// Copyright 2026 God Mode Games, LLC. All Rights Reserved.
// Node model of the agentbus mod's mid-turn delivery (register.ts, mod 0.4.1): the in-flight batch list
// `appending`, the first tool.call hook (take, append, turn-ended check, put-back, accept) and the
// turn.complete flush. It asserts the order prompts and rows come out in. Run: node <this file>.
import assert from 'node:assert/strict'
let inTurn, turnSeq = 0, midTurn, appending, out
const reset = () => { inTurn = true; turnSeq++; midTurn = []; appending = []; out = [] }
const submit = m => out.push(m)
async function hook(gate, ok) {
  if (midTurn.length === 0) return
  const msgs = midTurn.splice(0); appending.push(msgs); const seq = turnSeq
  await gate
  const at = appending.indexOf(msgs); if (at >= 0) appending.splice(at, 1)
  if (!inTurn || seq !== turnSeq) { for (const m of msgs.splice(0)) submit(m); return }
  if (!ok) { midTurn.unshift(...msgs); return }
  out.push(...msgs.map(m => m + '(row)'))
}
function complete() {
  inTurn = false
  for (const b of appending) for (const m of b.splice(0)) submit(m)
  appending.length = 0
  for (const m of midTurn.splice(0)) submit(m)
}
const gate = () => { let r; const p = new Promise(x => (r = x)); return [p, r] }

// 1: both appends in flight when the turn ends: arrival order.
reset(); { const [gA, rA] = gate(), [gB, rB] = gate()
  midTurn.push('a1'); const A = hook(gA, true); midTurn.push('b1'); const B = hook(gB, true); midTurn.push('c1')
  complete(); rA(); rB(); await A; await B
  assert.deepEqual(out, ['a1', 'b1', 'c1']); assert.equal(appending.length, 0) }
// 2: the newer batch is accepted first (a row in the turn), then the turn ends with the older in flight.
reset(); { const [gA, rA] = gate(), [gB, rB] = gate()
  midTurn.push('a1'); const A = hook(gA, true); midTurn.push('b1'); const B = hook(gB, true)
  rB(); await B; midTurn.push('c1'); complete(); rA(); await A
  assert.deepEqual(out, ['b1(row)', 'a1', 'c1']) }
// 3: the newer batch is refused (put back), then the turn ends with the older in flight.
reset(); { const [gA, rA] = gate(), [gB, rB] = gate()
  midTurn.push('a1'); const A = hook(gA, true); midTurn.push('b1'); const B = hook(gB, false)
  rB(); await B; midTurn.push('c1'); complete(); rA(); await A
  assert.deepEqual(out, ['a1', 'b1', 'c1']) }
// 4: the turn ends and the next starts before the old hook resumes: it runs nothing, the new turn holds.
reset(); { const [gA, rA] = gate()
  midTurn.push('a1'); const A = hook(gA, true); complete(); inTurn = true; turnSeq++; midTurn.push('n1')
  rA(); await A
  assert.deepEqual(out, ['a1']); assert.deepEqual(midTurn, ['n1']) }
// 5: three in flight, the middle one accepted first.
reset(); { const [gA, rA] = gate(), [gB, rB] = gate(), [gC, rC] = gate()
  midTurn.push('a1'); const A = hook(gA, true); midTurn.push('b1'); const B = hook(gB, true); midTurn.push('c1'); const C = hook(gC, true)
  rB(); await B; midTurn.push('d1'); complete(); rA(); rC(); await A; await C
  assert.deepEqual(out, ['b1(row)', 'a1', 'c1', 'd1']); assert.equal(appending.length, 0) }
// 6: a new turn starts without a turn.complete in between: the old batch runs when its hook resumes.
reset(); { const [gA, rA] = gate()
  midTurn.push('a1'); const A = hook(gA, true); turnSeq++; midTurn.push('n1')
  rA(); await A
  assert.deepEqual(out, ['a1']) }
// 7 (known, not ordered): the OLDER batch is refused while a newer one is in flight; the turn then ends.
reset(); { const [gA, rA] = gate(), [gB, rB] = gate()
  midTurn.push('a1'); const A = hook(gA, false); midTurn.push('b1'); const B = hook(gB, true)
  rA(); await A; midTurn.push('c1'); complete(); rB(); await B
  assert.deepEqual(out, ['b1', 'a1', 'c1']) }
// 8: a hook whose append never settles: turn.complete runs its batch and leaves no entry behind.
reset(); { const [gA] = gate()
  midTurn.push('a1'); void hook(gA, true); complete()
  assert.deepEqual(out, ['a1']); assert.equal(appending.length, 0) }
// 9: the next turn's hook pushes a batch while the old turn's hook is still in flight; then both settle.
reset(); { const [gA, rA] = gate(), [gN, rN] = gate()
  midTurn.push('a1'); const A = hook(gA, true); complete(); inTurn = true; turnSeq++
  midTurn.push('n1'); const N = hook(gN, true); rA(); await A; rN(); await N
  assert.deepEqual(out, ['a1', 'n1(row)']); assert.equal(appending.length, 0) }
console.log('agentbus mid-turn model: 9 cases ok')

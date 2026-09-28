#![no_std]
#![no_main]

extern crate alloc;

mod crypto;

use alloc::string::{String, ToString};
use alloc::vec;
use alloc::vec::Vec;
use core::alloc::{GlobalAlloc, Layout};

#[panic_handler]
fn panic(_info: &core::panic::PanicInfo) -> ! {
    unsafe {
        core::arch::asm!("unimp", options(noreturn));
    }
}

#[polkavm_derive::polkavm_import]
extern "C" {
    fn c0_gas() -> u64;
    fn c1_fetch(a1: u64, a2: u64, a3: u64, a4: u64, a5: u64, a6: u64) -> u64;
    fn c2_lookup(a1: u64, a2: u64, a3: u64, a4: u64, a5: u64, a6: u64) -> u64;
    fn c3_read(
        service: u64,
        key_addr: u64,
        key_len: u64,
        out_addr: u64,
        offset: u64,
        length: u64,
    ) -> u64;
    fn c4_write(key_addr: u64, key_len: u64, val_addr: u64, val_len: u64) -> u64;
    fn c5_info(a1: u64, a2: u64, a3: u64, a4: u64, a5: u64, a6: u64) -> u64;
}

const KEY_SUPPLY: u8 = 0x01;
const KEY_BALANCE: u8 = 0x02;
const KEY_NONCE: u8 = 0x03;
const KEY_EVMNONCE: u8 = 0x04;
const KEY_FAUCET: u8 = 0x05;
const KEY_WELCOME: u8 = 0x06;
const KEY_ISSUER: u8 = 0x07;

const DECIMALS: u8 = 12;
const MAX_SUPPLY: u128 = 1_000_000_000_000_000_000_000_000;
const TRANSFER_FEE: u128 = 1_000_000;
const FAUCET_AMOUNT: u128 = 10_000_000_000_000_000;
const WELCOME_AMOUNT: u128 = 5_000_000_000_000_000;
const FIRST_NONCE: u64 = 1;

const SELF: u64 = u64::MAX;

// Bump allocator over a static arena. The guest runs once per invocation and
// never returns, so free is a no-op and OOM is panic.
const HEAP_SIZE: usize = 64 * 1024;
#[repr(C, align(16))]
struct Arena([u8; HEAP_SIZE]);
#[used]
#[unsafe(link_section = ".data")]
static mut ARENA: Arena = Arena([0u8; HEAP_SIZE]);
#[used]
static mut OFFSET: usize = 0;

struct Bump;
unsafe impl GlobalAlloc for Bump {
    unsafe fn alloc(&self, layout: Layout) -> *mut u8 {
        let start = unsafe { OFFSET };
        let aligned = (start + layout.align().next_power_of_two()) & !(layout.align().next_power_of_two() - 1);
        let end = aligned + layout.size();
        if end > HEAP_SIZE {
            return core::ptr::null_mut();
        }
        unsafe {
            OFFSET = end;
            (ARENA.0.as_mut_ptr()).add(aligned)
        }
    }
    unsafe fn dealloc(&self, _ptr: *mut u8, _layout: Layout) {}
}

#[global_allocator]
static ALLOC: Bump = Bump;

fn storage_read(key: &[u8]) -> Option<Vec<u8>> {
    let out = vec![0u8; 8];
    let n = unsafe { c3_read(SELF, key.as_ptr() as u64, key.len() as u64, out.as_ptr() as u64, 0, u64::MAX) };
    if n == u64::MAX || n > 512 {
        return None;
    }
    let mut v = Vec::with_capacity(n as usize);
    unsafe {
        v.set_len(n as usize);
    }
    let got = unsafe { c3_read(SELF, key.as_ptr() as u64, key.len() as u64, v.as_mut_ptr() as u64, 0, n) };
    if got == u64::MAX || got != n {
        return None;
    }
    Some(v)
}

fn storage_write(key: &[u8], value: &[u8]) -> u64 {
    unsafe { c4_write(key.as_ptr() as u64, key.len() as u64, value.as_ptr() as u64, value.len() as u64) }
}

fn key_with(domain: u8, address: &str) -> Vec<u8> {
    let mut k = Vec::with_capacity(1 + address.len());
    k.push(domain);
    k.extend_from_slice(address.as_bytes());
    k
}

fn read_u128(key: &[u8]) -> u128 {
    match storage_read(key) {
        Some(v) if !v.is_empty() => {
            let mut x: u128 = 0;
            for b in v.iter() {
                x = (x << 8) | u128::from(*b);
            }
            x
        }
        _ => 0,
    }
}

fn write_u128(key: &[u8], value: u128) {
    let mut be = [0u8; 16];
    let mut v = value;
    for i in (0..16).rev() {
        be[i] = (v & 0xff) as u8;
        v >>= 8;
    }
    storage_write(key, &be);
}

fn read_nonce(address: &str) -> u64 {
    match storage_read(&key_with(KEY_NONCE, address)) {
        Some(v) if !v.is_empty() && v.len() <= 8 => {
            let mut x: u64 = 0;
            for b in v.iter() {
                x = (x << 8) | u64::from(*b);
            }
            x
        }
        _ => FIRST_NONCE,
    }
}

fn balance_of(address: &str) -> u128 {
    read_u128(&key_with(KEY_BALANCE, address))
}

fn credit(address: &str, amount: u128) {
    write_u128(&key_with(KEY_BALANCE, address), balance_of(address) + amount);
}

fn issuer() -> Option<Vec<u8>> {
    let stored = storage_read(&[KEY_ISSUER])?;
    if stored.is_empty() {
        None
    } else {
        Some(stored)
    }
}


fn parse_amount(input: &str) -> Option<u128> {
    let input = input.trim();
    if input.is_empty() || input.starts_with('+') {
        return None;
    }
    let has_dot = input.contains('.');
    let mut parts = input.splitn(2, '.');
    let whole = parts.next().unwrap_or("");
    let fraction = parts.next().unwrap_or("");
    if has_dot && fraction.len() > DECIMALS as usize {
        return None;
    }
    let mut digits = Vec::new();
    for ch in whole.bytes() {
        if !ch.is_ascii_digit() {
            return None;
        }
        digits.push(ch);
    }
    if whole.is_empty() && fraction.is_empty() {
        return None;
    }
    for ch in fraction.bytes() {
        if !ch.is_ascii_digit() {
            return None;
        }
        digits.push(ch);
    }
    for _ in fraction.len()..DECIMALS as usize {
        digits.push(b'0');
    }
    let mut v: u128 = 0;
    for ch in digits {
        v = v.checked_mul(10)?.checked_add(u128::from(ch - b'0'))?;
    }
    Some(v)
}

// parse_raw reads a raw base-10 integer of already-scaled units, which is the
// form a refine report carries. It must not rescale: a report's amount is
// already in smallest units, unlike an item's decimal amount.
fn parse_raw(input: &str) -> Option<u128> {
    let input = input.trim();
    if input.is_empty() {
        return None;
    }
    let mut v: u128 = 0;
    for ch in input.bytes() {
        if !ch.is_ascii_digit() {
            return None;
        }
        v = v.checked_mul(10)?.checked_add(u128::from(ch - b'0'))?;
    }
    Some(v)
}

fn raw_string(raw: u128) -> String {
    if raw == 0 {
        return "0".to_string();
    }
    let mut s = String::new();
    let mut v = raw;
    while v > 0 {
        s.insert(0, char::from(b'0' + (v % 10) as u8));
        v /= 10;
    }
    s
}

// ---------- refine ----------

#[derive(serde::Deserialize)]
struct Item {
    method: String,
    sender: String,
    nonce: u64,
    #[serde(default)]
    to: Option<String>,
    #[serde(default)]
    amount: Option<String>,
    #[serde(default)]
    memo: Option<String>,
    #[serde(default)]
    raw: Option<String>,
    #[serde(default, rename = "mustBeSigned")]
    must_be_signed: bool,
    #[serde(default)]
    signature: Option<String>,
}

fn json_field(name: &str, value: &str, out: &mut Vec<u8>) {
    out.push(b'"');
    out.extend_from_slice(name.as_bytes());
    out.extend_from_slice(b"\":\"");
    for b in value.bytes() {
        match b {
            b'"' => out.extend_from_slice(b"\\\""),
            b'\\' => out.extend_from_slice(b"\\\\"),
            _ => out.push(b),
        }
    }
    out.push(b'"');
}

fn push_field(name: &str, value: &str, first: &mut bool, out: &mut Vec<u8>) {
    if !value.is_empty() {
        if !*first {
            out.push(b',');
        }
        json_field(name, value, out);
        *first = false;
    }
}

fn refine_item(args: &[u8]) -> Option<Vec<u8>> {
    let item: Item = serde_json::from_slice(args).ok()?;

    match item.method.as_str() {
        "transfer" | "mint" | "burn" | "faucet" | "welcome" => {}
        _ => return None,
    }
    if item.raw.is_some() {
        return None; // the EVM relay lands in a later phase
    }

    let sender = crypto::normalize_address(&item.sender)?;
    if item.must_be_signed {
        // The signature covers the sender, so a signed item can only ever act
        // for the account that signed it. An unprovable sender gets no report.
        let public_key = crypto::validate_chain_address(&item.sender)?;
        let signature = crypto::decode_hex(item.signature.as_deref().unwrap_or(""))?;
        let payload = crypto::signable_payload(
            &item.method,
            item.nonce,
            &item.sender,
            item.to.as_deref().unwrap_or(""),
            item.amount.as_deref().unwrap_or(""),
            item.memo.as_deref().unwrap_or(""),
            item.must_be_signed,
        );
        if !crypto::verify_signature(&public_key, &payload, &signature) {
            return None;
        }
    }
    let to = match &item.to {
        Some(t) => Some(crypto::normalize_address(t)?),
        None => None,
    };

    let mut report = Vec::with_capacity(160);
    report.push(b'{');
    let mut first = true;
    push_field("op", &item.method, &mut first, &mut report);
    push_field("actor", &sender, &mut first, &mut report);
    push_field("sender", &sender, &mut first, &mut report);
    report.push(b',');
    report.extend_from_slice(b"\"nonce\":");
    report.extend_from_slice(raw_string(item.nonce as u128).as_bytes());
    first = false;

    match item.method.as_str() {
        "transfer" | "mint" => {
            let to = to?;
            let amount = parse_amount(item.amount.as_ref()?)?;
            if amount == 0 {
                return None;
            }
            let raw = raw_string(amount);
            push_field("to", &to, &mut first, &mut report);
            push_field("amount", &raw, &mut first, &mut report);
            if item.method == "transfer" {
                push_field("fee", "1000000", &mut first, &mut report);
            }
        }
        "burn" => {
            let amount = parse_amount(item.amount.as_ref()?)?;
            if amount == 0 {
                return None;
            }
            push_field("amount", &raw_string(amount), &mut first, &mut report);
        }
        _ => {
            let t = to.unwrap_or_else(|| sender.clone());
            push_field("to", &t, &mut first, &mut report);
        }
    }
    if let Some(memo) = &item.memo {
        push_field("memo", memo, &mut first, &mut report);
    }
    report.push(b'}');
    Some(report)
}

// ---------- accumulate ----------

#[derive(serde::Deserialize)]
struct Op {
    op: String,
    actor: String,
    sender: String,
    nonce: u64,
    #[serde(default)]
    to: String,
    #[serde(default)]
    amount: String,
    #[serde(default)]
    fee: String,
    #[serde(default)]
    memo: String,
    #[serde(default)]
    evm_nonce: Option<u64>,
}

fn advance_nonce(actor: &str, nonce: u64) {
    write_u128(&key_with(KEY_NONCE, actor), nonce as u128);
}

fn apply_op(op: &Op) -> bool {
    let expected = read_nonce(&op.actor);
    if op.nonce != expected {
        return false;
    }

    match op.op.as_str() {
        "transfer" => {
            let amount = parse_raw(&op.amount).unwrap_or(0);
            let to = crypto::normalize_address(&op.to).unwrap_or_default();
            let from = balance_of(&op.sender);
            let total = amount + TRANSFER_FEE;
            if from < total {
                return false;
            }
            write_u128(&key_with(KEY_BALANCE, &op.sender), from - total);
            credit(&to, amount);
            let supply = read_u128(&[KEY_SUPPLY]);
            let next = if supply >= TRANSFER_FEE { supply - TRANSFER_FEE } else { 0 };
            write_u128(&[KEY_SUPPLY], next);
            advance_nonce(&op.actor, op.nonce + 1);
            true
        }
        "mint" => {
            match issuer() {
                None => return false,
                Some(iss) if iss.as_slice() != op.actor.as_bytes() => return false,
                Some(_) => {}
            }
            let amount = parse_raw(&op.amount).unwrap_or(0);
            let supply = read_u128(&[KEY_SUPPLY]);
            if supply + amount > MAX_SUPPLY {
                return false;
            }
            credit(&op.to, amount);
            write_u128(&[KEY_SUPPLY], supply + amount);
            advance_nonce(&op.actor, op.nonce + 1);
            true
        }
        "burn" => {
            let amount = parse_raw(&op.amount).unwrap_or(0);
            let from = balance_of(&op.sender);
            if from < amount {
                return false;
            }
            write_u128(&key_with(KEY_BALANCE, &op.sender), from - amount);
            let supply = read_u128(&[KEY_SUPPLY]);
            write_u128(&[KEY_SUPPLY], if supply >= amount { supply - amount } else { 0 });
            advance_nonce(&op.actor, op.nonce + 1);
            true
        }
        "faucet" | "welcome" => {
            let kind = if op.op == "faucet" { KEY_FAUCET } else { KEY_WELCOME };
            let claim = key_with(kind, &op.to);
            if storage_read(&claim).map_or(false, |v| !v.is_empty()) {
                return false;
            }
            let amount = if kind == KEY_FAUCET { FAUCET_AMOUNT } else { WELCOME_AMOUNT };
            let supply = read_u128(&[KEY_SUPPLY]);
            if supply + amount > MAX_SUPPLY {
                return false;
            }
            credit(&op.to, amount);
            write_u128(&[KEY_SUPPLY], supply + amount);
            storage_write(&claim, &[1]);
            advance_nonce(&op.actor, op.nonce + 1);
            true
        }
        _ => false,
    }
}

fn accumulate_reports(args: &[u8]) {
    let body = if args.last() == Some(&0) { &args[..args.len() - 1] } else { args };
    if body.is_empty() {
        return;
    }
    let mut de = serde_json::Deserializer::from_slice(body);
    use serde::Deserialize;
    while let Ok(op) = Op::deserialize(&mut de) {
        apply_op(&op);
    }
}

#[polkavm_derive::polkavm_export]
#[no_mangle]
pub extern "C" fn main(_args_addr: u64, _args_len: u64) -> u32 {
    let args = read_args(_args_addr, _args_len);

    if args.last() == Some(&0) {
        accumulate_reports(args);
        halt(0, 0);
    }
    if let Some(r) = refine_item(args) {
        halt(r.as_ptr() as u64, r.len() as u64);
    }
    halt(0, 0)
}

fn read_args<'a>(base: u64, len: u64) -> &'a [u8] {
    if len == 0 || base > u32::MAX as u64 {
        return &[];
    }
    unsafe { core::slice::from_raw_parts(base as *const u8, len as usize) }
}

#[inline(never)]
fn halt(addr: u64, len: u64) -> ! {
    unsafe {
        core::arch::asm!(
            "mv a0, {0}",
            "mv a1, {1}",
            "li ra, -65536",
            "ret",
            in(reg) addr,
            in(reg) len,
            options(noreturn),
        );
    }
}

#[allow(dead_code)]
fn _touch() {
    let _ = unsafe { c0_gas() };
    let _ = unsafe { c1_fetch(0, 0, 0, 0, 0, 0) };
    let _ = unsafe { c2_lookup(0, 0, 0, 0, 0, 0) };
    let _ = unsafe { c5_info(0, 0, 0, 0, 0, 0) };
}
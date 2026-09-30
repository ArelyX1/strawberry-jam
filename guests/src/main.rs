#![no_std]
#![no_main]

extern crate alloc;

use papucoin_core::{crypto, evm};

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
const KEY_ASSET: u8 = 0x08;
const KEY_FEE_LEVEL: u8 = 0x0a;

const DECIMALS: u8 = 12;
const MAX_SUPPLY: u128 = 1_000_000_000_000_000_000_000_000;
const TRANSFER_FEE: u128 = 1_000_000;
const FAUCET_AMOUNT: u128 = 10_000_000_000_000_000;
const WELCOME_AMOUNT: u128 = 5_000_000_000_000_000;
const FIRST_NONCE: u64 = 1;

// The fee is a price, not a tariff, and these are its limits. They are the same
// numbers the native service uses, and they have to be: a chain that runs either
// one has to charge the same figure, or a client that quoted the other is
// quoted wrong. All of it is far below one PAPU, because twelve decimals make a
// whole unit a million times too coarse to be a fee.
const FEE_FLOOR: u128 = 1_000;
const FEE_CEILING: u128 = 1_000_000_000;
const FEE_BUSY_TARGET: u32 = 8;
const FEE_UP_NUMERATOR: u128 = 3;
const FEE_UP_DENOMINATOR: u128 = 2;
const FEE_DOWN_NUMERATOR: u128 = 2;
const FEE_DOWN_DENOMINATOR: u128 = 3;

const SELF: u64 = u64::MAX;

// Bump allocator over a static arena. The guest runs once per invocation and
// never returns, so free is a no-op and OOM is panic.
const HEAP_SIZE: usize = 256 * 1024;
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
        let arena = core::ptr::addr_of_mut!(ARENA.0) as *mut u8;
        let offset = core::ptr::addr_of_mut!(OFFSET);
        let start = unsafe { *offset };
        let align = layout.align();
        let aligned = (start + align - 1) & !(align - 1);
        let end = aligned + layout.size();
        if end > HEAP_SIZE {
            return core::ptr::null_mut();
        }
        unsafe {
            *offset = end;
            arena.add(aligned)
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

// A count is stored the same way an amount is, in sixteen big endian octets, so
// it is read back the same way. Refusing anything wider than eight would make
// the value this module writes unreadable to itself, and the nonce would never
// move past the first one an account used.
fn read_count(key: &[u8], absent: u64) -> u64 {
    match storage_read(key) {
        Some(v) if !v.is_empty() && v.len() <= 16 => {
            let mut x: u64 = 0;
            for b in v.iter() {
                x = (x << 8) | u64::from(*b);
            }
            x
        }
        _ => absent,
    }
}

fn read_nonce(address: &str) -> u64 {
    read_count(&key_with(KEY_NONCE, address), FIRST_NONCE)
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
    // EVMNonce and EVMSender are filled in by the chain, not by the wallet: the
    // host verifies the signature and states who it recovered.
    #[serde(default, rename = "evmSender")]
    evm_sender: Option<String>,
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

// KEY_CHAIN_ID is where the chain states which EVM chain it presents, so a
// transaction signed for another one cannot be replayed here. It is written by
// the seed, because the chain id is a property of the chain rather than of this
// program.
const KEY_CHAIN_ID: u8 = 0x09;
const WEI_GRANULARITY: u128 = 1_000_000;

fn chain_id() -> u64 {
    match storage_read(&[KEY_CHAIN_ID]) {
        Some(v) if v.len() == 8 => {
            let mut id: u64 = 0;
            for b in v.iter() {
                id = (id << 8) | u64::from(*b);
            }
            id
        }
        // A chain that never said what it is accepts nothing relayed, which is
        // the safe answer: guessing would let any signed transaction through.
        _ => 0,
    }
}

// refine_relayed turns a raw EVM transaction into a report. The sender is
// recovered from the signature, so nothing the item claims about who sent it is
// believed; the destination still has to agree with the signed bytes, otherwise
// a valid transaction could be pointed at someone else's balance.
fn recovered_sender(item: &Item) -> Option<[u8; 20]> {
    // The chain verifies the signature before the item gets here and states the
    // recovered address in evmSender. Nothing here re-derives it, and nothing
    // here would believe the claimed sender if the host had not checked it.
    let raw = evm::decode_hex_address(item.evm_sender.as_deref()?)?;
    Some(raw)
}

fn refine_relayed(raw_hex: &str, item: &Item) -> Option<Vec<u8>> {
    let raw = match crypto::decode_hex(raw_hex) {
        Some(raw) => raw,
        None => return Some(br#"{"relayError":"la transaccion no es hex"}"#.to_vec()),
    };
    let tx = match evm::decode_relayed(&raw, chain_id(), recovered_sender(item)) {
        Ok(tx) => tx,
        Err(reason) => {
            let mut out = Vec::with_capacity(reason.len() + 24);
            out.extend_from_slice(b"{\"relayError\":\"");
            out.extend_from_slice(reason.as_bytes());
            out.extend_from_slice(b"\"}");
            return Some(out);
        }
    };

    let from = evm_address(tx.from);
    let to = evm_address(tx.to);

    if let Some(destination) = &item.to {
        if crypto::normalize_address(destination)? != to {
            return Some(br#"{"relayError":"el destino no coincide con la firma"}"#.to_vec());
        }
    }

    if tx.value % WEI_GRANULARITY != 0 {
        return Some(br#"{"relayError":"el valor no es un numero entero de micro-PAPU"}"#.to_vec());
    }
    let amount = tx.value / WEI_GRANULARITY;
    if amount == 0 {
        return Some(br#"{"relayError":"el valor es cero"}"#.to_vec());
    }

    let mut report = Vec::with_capacity(200);
    report.push(b'{');
    crypto::json_field("op", "transfer", &mut report);
    report.push(b',');
    crypto::json_field("actor", &from, &mut report);
    report.push(b',');
    crypto::json_field("sender", &from, &mut report);
    report.push(b',');
    report.extend_from_slice(b"\"nonce\":");
    report.extend_from_slice(raw_string(item.nonce as u128).as_bytes());
    report.push(b',');
    crypto::json_field("to", &to, &mut report);
    report.push(b',');
    crypto::json_field("amount", &raw_string(amount), &mut report);
    report.push(b',');
    crypto::json_field("fee", "1000000", &mut report);
    report.push(b',');
    report.extend_from_slice(b"\"evmNonce\":");
    report.extend_from_slice(raw_string(tx.evm_nonce as u128).as_bytes());
    report.push(b'}');
    Some(report)
}

fn evm_address(bytes: [u8; 20]) -> String {
    let mut s = String::with_capacity(42);
    s.push_str("0x");
    for b in bytes {
        s.push(char::from(b"0123456789abcdef"[(b >> 4) as usize]));
        s.push(char::from(b"0123456789abcdef"[(b & 0xf) as usize]));
    }
    s
}

fn refine_item(args: &[u8]) -> Option<Vec<u8>> {
    let item: Item = match serde_json::from_slice(args) {
        Ok(item) => item,
        Err(_) => {
            // A malformed item is reported so the reason is visible instead of
            // arriving as a silent refusal.
            return Some(br#"{"parseError":"el item no es json valido"}"#.to_vec());
        }
    };

    match item.method.as_str() {
        "transfer" | "mint" | "burn" | "faucet" | "welcome" => {}
        _ => return None,
    }
    if let Some(raw) = &item.raw {
        if !evm::selfcheck() {
            return Some(br#"{"relayError":"keccak no coincide con los vectores"}"#.to_vec());
        }

        let raw_bytes = match crypto::decode_hex(raw) {
            Some(b) => b,
            None => return Some(br#"{"relayError":"la transaccion no es hex valido"}"#.to_vec()),
        };
        if let Err(reason) = evm::decode_relayed(&raw_bytes, chain_id(), recovered_sender(&item)) {
            // The reason is reported rather than swallowed: a wallet that
            // cannot get a transfer through needs to know whether the
            // transaction was malformed or the signature did not recover.
            let mut out = Vec::with_capacity(64);
            out.push(b'{');
            crypto::json_field("relayError", reason, &mut out);
            out.push(b'}');
            return Some(out);
        }
        return refine_relayed(raw, &item);
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

fn read_evm_nonce(address: &str) -> u64 {
    read_count(&key_with(KEY_EVMNONCE, address), 0)
}

fn advance_evm_nonce(address: &str, nonce: u64) {
    write_u128(&key_with(KEY_EVMNONCE, address), nonce as u128);
}

// current_fee is the price a block charges. A chain that has never charged one
// falls back to what its genesis named, so the first transfer costs what the
// chain was configured with rather than nothing.
fn current_fee() -> u128 {
    match storage_read(&[KEY_FEE_LEVEL]) {
        Some(v) if !v.is_empty() => {
            let level = read_u128(&[KEY_FEE_LEVEL]);
            if level < FEE_FLOOR {
                FEE_FLOOR
            } else if level > FEE_CEILING {
                FEE_CEILING
            } else {
                level
            }
        }
        _ => TRANSFER_FEE,
    }
}

// next_fee moves the price in the direction the block's load asks for. It runs
// once per block, after that block's transfers, so what a block is charged is
// the price the one before it settled on and a block never charges a price that
// has not been published.
fn next_fee(transfers: u32) {
    let current = current_fee();
    let next = if transfers > FEE_BUSY_TARGET {
        current * FEE_UP_NUMERATOR / FEE_UP_DENOMINATOR
    } else if transfers == 0 {
        current * FEE_DOWN_NUMERATOR / FEE_DOWN_DENOMINATOR
    } else {
        current
    };
    let clamped = if next < FEE_FLOOR {
        FEE_FLOOR
    } else if next > FEE_CEILING {
        FEE_CEILING
    } else {
        next
    };
    write_u128(&[KEY_FEE_LEVEL], clamped);
}

fn apply_op(op: &Op) -> bool {
    // A number that has already gone by is a replay and is dropped. A number at
    // or ahead of the one the account is on is taken, and the account moves to
    // one past it, so the same signed item can never be taken twice.
    //
    // Demanding the exact number only works while nothing else is queued for the
    // account at the same moment. An account that sends three transfers before the
    // next block has them all numbered from the same state, the chain keeps the
    // first and silently drops the other two, and the money in them is gone with
    // no error anywhere. Allowing a number ahead costs the ability to jump the
    // queue by more than the number of items already waiting, and no third party
    // can do it: a relayed item is numbered by the node and a chain item has to be
    // signed by the account it moves money for.
    let expected = read_nonce(&op.actor);
    if op.nonce < expected {
        return false;
    }

    match op.op.as_str() {
        "transfer" => {
            let amount = parse_raw(&op.amount).unwrap_or(0);
            let to = crypto::normalize_address(&op.to).unwrap_or_default();
            // An EVM transaction carries its own nonce, and the chain checks it
            // against the count of the ones it has already taken from this
            // sender. Without this a signed transaction could be put back on
            // the wire as many times as an observer liked and each copy would
            // move the money again.
            if let Some(n) = op.evm_nonce {
                if n != read_evm_nonce(&op.sender) {
                    return false;
                }
            }
            let from = balance_of(&op.sender);
            // The fee is the one this report was refined at, which is the
            // figure a client was shown. Charging the live price instead would
            // be charging something nobody was told.
            let fee = clamp_fee(parse_raw(&op.fee).unwrap_or(0));
            let total = amount + fee;
            if from < total {
                return false;
            }
            write_u128(&key_with(KEY_BALANCE, &op.sender), from - total);
            credit(&to, amount);
            let supply = read_u128(&[KEY_SUPPLY]);
            let next = if supply >= fee { supply - fee } else { 0 };
            write_u128(&[KEY_SUPPLY], next);
            if op.evm_nonce.is_some() {
                advance_evm_nonce(&op.sender, op.evm_nonce.unwrap() + 1);
            }
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

fn null_as_empty_map<'de, D>(
    deserializer: D,
) -> Result<alloc::collections::BTreeMap<String, String>, D::Error>
where
    D: serde::Deserializer<'de>,
{
    use serde::Deserialize;
    Ok(Option::<alloc::collections::BTreeMap<String, String>>::deserialize(deserializer)?.unwrap_or_default())
}

// seed is what the chain hands over so the economy can write its genesis state.
// The native service receives it by running its own seed handler; a guest has no
// such handler, so the chain passes it in.
#[derive(serde::Deserialize)]
struct Seed {
    #[serde(rename = "Issuer")]
    issuer: String,
    #[serde(rename = "Symbol")]
    symbol: String,
    // The host sends balances as a JSON object, address to amount, so that is
    // what is read here; an array of pairs would be a second spelling of the
    // same thing and the chain would not agree on which one it is.
    // A nil map arrives as JSON null, which a BTreeMap will not take; a chain
    // with nobody funded is a chain with an empty set, not a broken one.
    #[serde(default, deserialize_with = "null_as_empty_map", rename = "Balances")]
    balances: alloc::collections::BTreeMap<String, String>,
    // ChainID is optional so a seed written before the chain said what it is
    // still seeds the rest; the relayed path stays closed until it does.
    #[serde(default, rename = "ChainID")]
    chain_id: u64,
}

fn write_seed(args: &[u8]) {
    let seed: Seed = match serde_json::from_slice(args) {
        Ok(seed) => seed,
        Err(_) => return,
    };
    // The issuer, the asset name and the chain id are written whatever the seed
    // carries in balances: an empty opening set is a chain that starts with
    // nobody funded, not a reason to write nothing at all.
    if !seed.issuer.is_empty() {
        storage_write(&[KEY_ISSUER], seed.issuer.as_bytes());
    }
    if !seed.symbol.is_empty() {
        storage_write(&[KEY_ASSET], seed.symbol.as_bytes());
    }
    if seed.chain_id != 0 {
        let mut be = [0u8; 8];
        let mut v = seed.chain_id;
        for i in (0..8).rev() {
            be[i] = (v & 0xff) as u8;
            v >>= 8;
        }
        storage_write(&[KEY_CHAIN_ID], &be);
    }
    storage_write(&[KEY_ASSET], seed.symbol.as_bytes());

    let mut total: u128 = 0;
    for (address, amount) in seed.balances.iter() {
        let raw = match parse_amount(amount) {
            Some(raw) => raw,
            None => continue,
        };
        total += raw;
        let key = key_with(KEY_BALANCE, address);
        write_u128(&key, raw);
    }
    if total > MAX_SUPPLY {
        total = MAX_SUPPLY;
    }
    write_u128(&[KEY_SUPPLY], total);
}

fn accumulate_reports(args: &[u8]) {
    let body = if args.last() == Some(&0) { &args[..args.len() - 1] } else { args };

    // An empty block is still a block, and it is still evidence of how busy the
    // chain is. Returning early here would mean the price only ever came down
    // on a block that happened to carry some other item, which is a different
    // rule from the one the native service follows and would leave the two
    // implementations standing at different prices after the same history.
    let mut transfers = 0u32;
    if !body.is_empty() {
        let mut de = serde_json::Deserializer::from_slice(body);
        use serde::Deserialize;
        while let Ok(op) = Op::deserialize(&mut de) {
            if apply_op(&op) && op.op == "transfer" {
                transfers += 1;
            }
        }
    }
    // The block's load is what moves the price, so the move happens here, after
    // the work, and is what the next block will charge.
    next_fee(transfers);
}

#[polkavm_derive::polkavm_export]
#[no_mangle]
pub extern "C" fn main(_args_addr: u64, _args_len: u64) -> u32 {
    let args = read_args(_args_addr, _args_len);
    if args.last() == Some(&0) {
        let body = &args[..args.len() - 1];
        // A seed is an object rather than a stream of reports, and the chain
        // only sends one, at the very start.
        if body.first() == Some(&b'{') {
            if serde_json::from_slice::<Seed>(body).is_ok() {
                write_seed(body);
                halt(0, 0);
            }
        }
        accumulate_reports(body);
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
// clamp_fee keeps a quoted fee inside the band the chain allows, so a report
// that arrives with a figure outside it is charged the nearest figure that is
// inside rather than whatever it asked for.
fn clamp_fee(fee: u128) -> u128 {
    if fee < FEE_FLOOR {
        FEE_FLOOR
    } else if fee > FEE_CEILING {
        FEE_CEILING
    } else {
        fee
    }
}

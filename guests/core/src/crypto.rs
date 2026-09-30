use alloc::string::{String, ToString};
use alloc::vec::Vec;

pub const SDLG_PREFIX: &str = "sdlg";
pub const DECODED_ADDRESS_LEN: usize = 36;
const B58: &[u8] = b"123456789ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz";

fn b58_value(c: u8) -> Option<u8> {
    B58.iter().position(|&x| x == c).map(|p| p as u8)
}

// base58_decode mirrors the native decoder, including its rejection of
// characters outside the alphabet so a typo fails instead of decoding to a
// different key.
fn base58_decode(input: &[u8]) -> Option<Vec<u8>> {
    if input.is_empty() {
        return None;
    }
    let mut acc: Vec<u8> = Vec::new();
    for &c in input {
        let digit = b58_value(c)?;
        let mut carry = digit as u32;
        for byte in acc.iter_mut().rev() {
            let v = (*byte as u32) * 58 + carry;
            *byte = (v & 0xff) as u8;
            carry = v >> 8;
        }
        while carry > 0 {
            acc.insert(0, (carry & 0xff) as u8);
            carry >>= 8;
        }
    }
    let leading = input.iter().take_while(|&&c| c == B58[0]).count();
    if acc.is_empty() {
        acc.push(0);
    }
    let mut out = Vec::with_capacity(leading + acc.len());
    for _ in 0..leading {
        out.push(0);
    }
    out.extend_from_slice(&acc);
    Some(out)
}

fn blake2b_256(input: &[u8]) -> [u8; 32] {
    use blake2::digest::{consts::U32, Digest};
    let mut hasher = blake2::Blake2b::<U32>::new();
    hasher.update(input);
    let out = hasher.finalize();
    let mut result = [0u8; 32];
    result.copy_from_slice(&out);
    result
}

pub fn address_checksum(public_key: &[u8]) -> [u8; 4] {
    let sum = blake2b_256(public_key);
    [sum[0], sum[1], sum[2], sum[3]]
}

// validate_chain_address returns the ed25519 public key behind a chain address,
// checking the blake2b checksum suffix.
pub fn validate_chain_address(input: &str) -> Option<[u8; 32]> {
    let body = input.strip_prefix(SDLG_PREFIX)?;
    let decoded = base58_decode(body.as_bytes())?;
    if decoded.len() != DECODED_ADDRESS_LEN {
        return None;
    }
    let mut public_key = [0u8; 32];
    public_key.copy_from_slice(&decoded[..32]);
    if address_checksum(&public_key) != decoded[32..36] {
        return None;
    }
    Some(public_key)
}

pub fn is_evm_address(input: &str) -> bool {
    input.len() == 42
        && (input.starts_with("0x") || input.starts_with("0X"))
        && input[2..].bytes().all(|b| b.is_ascii_hexdigit())
}

// normalize_address matches the native rules: an EVM address is lowercased, a
// chain address keeps its case because base58 is case sensitive.
pub fn normalize_address(input: &str) -> Option<String> {
    let v = input.trim();
    if v.is_empty() {
        return None;
    }
    if is_evm_address(v) {
        return Some(v.to_ascii_lowercase());
    }
    if validate_chain_address(v).is_some() {
        return Some(v.to_string());
    }
    None
}

pub fn decode_hex(input: &str) -> Option<Vec<u8>> {
    let body = input.trim();
    let body = body.strip_prefix("0x").or_else(|| body.strip_prefix("0X"))?;
    if body.len() % 2 != 0 {
        return None;
    }
    let bytes = body.as_bytes();
    let mut out = Vec::with_capacity(body.len() / 2);
    let mut i = 0;
    while i < bytes.len() {
        let hi = (bytes[i] as char).to_digit(16)?;
        let lo = (bytes[i + 1] as char).to_digit(16)?;
        out.push(((hi << 4) | lo) as u8);
        i += 2;
    }
    Some(out)
}

pub const SIGNATURE_DOMAIN: &str = "SDLG-PAPU::1";

// signable_payload rebuilds the exact bytes a signature covers, matching the
// native struct field order and its omitempty behaviour.
pub fn signable_payload(
    method: &str,
    nonce: u64,
    sender: &str,
    to: &str,
    amount: &str,
    memo: &str,
    must_be_signed: bool,
) -> Vec<u8> {
    let mut out = Vec::with_capacity(160);
    out.extend_from_slice(SIGNATURE_DOMAIN.as_bytes());
    out.push(b'{');
    json_field("method", method, &mut out);
    out.push(b',');
    out.extend_from_slice(b"\"nonce\":");
    out.extend_from_slice(raw_string(nonce as u128).as_bytes());
    out.push(b',');
    json_field("sender", sender, &mut out);
    if !to.is_empty() {
        out.push(b',');
        json_field("to", to, &mut out);
    }
    if !amount.is_empty() {
        out.push(b',');
        json_field("amount", amount, &mut out);
    }
    if !memo.is_empty() {
        out.push(b',');
        json_field("memo", memo, &mut out);
    }
    out.push(b',');
    out.extend_from_slice(b"\"mustBeSigned\":");
    out.extend_from_slice(if must_be_signed { b"true" } else { b"false" });
    out.push(b'}');
    out
}

pub fn json_field(name: &str, value: &str, out: &mut Vec<u8>) {
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

pub fn raw_string(raw: u128) -> String {
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

// verify_signature checks an ed25519 signature over payload for public_key.
// Verification is the whole point of mustBeSigned, so anything unparseable is
// a rejection rather than a skip.
pub fn verify_signature(public_key: &[u8; 32], payload: &[u8], signature: &[u8]) -> bool {
    use ed25519_dalek::{Signature, Verifier, VerifyingKey};
    let Ok(key) = VerifyingKey::from_bytes(public_key) else {
        return false;
    };
    if signature.len() != 64 {
        return false;
    }
    let mut raw = [0u8; 64];
    raw.copy_from_slice(signature);
    let sig = Signature::from_bytes(&raw);
    key.verify(payload, &sig).is_ok()
}

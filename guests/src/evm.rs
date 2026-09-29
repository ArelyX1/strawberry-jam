use alloc::vec::Vec;

// rlp_decode_list flattens an RLP list payload into its items. A list is a
// sequence of nested items, so callers walk the result as a tree by re-reading
// each item's own prefix.
pub fn rlp_split(payload: &[u8]) -> Option<Vec<(usize, usize)>> {
    let mut out = Vec::new();
    let mut p = 0;
    while p < payload.len() {
        let (start, len) = rlp_item(payload, p)?;
        out.push((start, len));
        p = start + len;
    }
    Some(out)
}

// rlp_item decodes one item header at p, returning where its payload starts and
// how long the payload is.
pub fn rlp_item(data: &[u8], p: usize) -> Option<(usize, usize)> {
    let prefix = *data.get(p)?;
    if prefix < 0x80 {
        return Some((p, 1));
    }
    if prefix <= 0xb7 {
        let len = (prefix - 0x80) as usize;
        if len == 1 {
            let only = *data.get(p + 1)?;
            if only < 0x80 {
                // A single byte below 0x80 is its own encoding, not a string.
                return None;
            }
        }
        return Some((p + 1, len));
    }
    if prefix <= 0xbf {
        let len_of_len = (prefix - 0xb7) as usize;
        let mut len: usize = 0;
        for i in 0..len_of_len {
            len = (len << 8) | *data.get(p + 1 + i)? as usize;
        }
        return Some((p + 1 + len_of_len, len));
    }
    // Lists are returned as their payload; the caller decides how to read it.
    if prefix <= 0xf7 {
        let len = (prefix - 0xc0) as usize;
        return Some((p + 1, len));
    }
    let len_of_len = (prefix - 0xf7) as usize;
    let mut len: usize = 0;
    for i in 0..len_of_len {
        len = (len << 8) | *data.get(p + 1 + i)? as usize;
    }
    Some((p + 1 + len_of_len, len))
}

pub fn rlp_is_list(data: &[u8], p: usize) -> bool {
    data.get(p).map_or(false, |b| *b >= 0xc0)
}

pub fn rlp_payload<'a>(data: &'a [u8], p: usize) -> Option<&'a [u8]> {
    let (start, len) = rlp_item(data, p)?;
    data.get(start..start + len)
}

// rlp_uint reads a big-endian integer, ignoring leading zero bytes as the spec
// requires.
pub fn rlp_uint(data: &[u8], p: usize) -> Option<u64> {
    let payload = rlp_bytes(data, p)?;
    if payload.is_empty() {
        return Some(0);
    }
    if payload.len() > 8 {
        return None;
    }
    if payload[0] == 0 {
        return None;
    }
    let mut v: u64 = 0;
    for b in payload {
        v = (v << 8) | *b as u64;
    }
    Some(v)
}

pub fn rlp_bytes<'a>(data: &'a [u8], p: usize) -> Option<&'a [u8]> {
    let (start, len) = rlp_item(data, p)?;
    if start == p && len == 1 {
        // single byte encoding, payload is the byte itself
        return data.get(p..p + 1);
    }
    data.get(start..start + len)
}

// keccak256 is the EVM hash, not blake2b. It is keccak as Ethereum uses it,
// which predates the final SHA-3 padding, so it needs its own implementation.
mod keccak {
    const ROUNDS: usize = 24;
    const RATE: usize = 136;

    const RC: [u64; ROUNDS] = [
        0x0000000000000001,
        0x0000000000008082,
        0x800000000000808a,
        0x8000000080008000,
        0x000000000000808b,
        0x0000000080000001,
        0x8000000080008081,
        0x8000000000008009,
        0x000000000000008a,
        0x0000000000000088,
        0x0000000080008009,
        0x000000008000000a,
        0x000000008000808b,
        0x800000000000008b,
        0x8000000000008089,
        0x8000000000008003,
        0x8000000000008002,
        0x8000000000000080,
        0x000000000000800a,
        0x800000008000000a,
        0x8000000080008081,
        0x8000000000008080,
        0x0000000080000001,
        0x8000000080008008,
    ];

    const ROTC: [u32; 24] = [
        1, 3, 6, 10, 15, 21, 28, 36, 45, 55, 2, 14, 27, 41, 56, 8, 25, 43, 62, 18, 39, 61, 20, 44,
    ];

    const PILN: [usize; 24] = [
        10, 7, 11, 17, 18, 3, 5, 16, 8, 21, 24, 4, 15, 23, 19, 13, 12, 2, 20, 14, 22, 9, 6, 1,
    ];

    fn keccak_f(state: &mut [u64; 25]) {
        for round in 0..ROUNDS {
            // theta
            let mut c = [0u64; 5];
            for x in 0..5 {
                c[x] = state[x] ^ state[x + 5] ^ state[x + 10] ^ state[x + 15] ^ state[x + 20];
            }
            for x in 0..5 {
                let d = c[(x + 4) % 5] ^ c[(x + 1) % 5].rotate_left(1);
                for y in 0..5 {
                    state[x + 5 * y] ^= d;
                }
            }

            // rho and pi: the lane at (x, y) moves to (y, 2x + 3y) while being
            // rotated, which is the single pass both steps describe.
            let mut t = state[1];
            for i in 0..24 {
                let j = PILN[i];
                let lane = state[j];
                state[j] = t.rotate_left(ROTC[i]);
                t = lane;
            }

            // chi
            for y in 0..5 {
                let row = [
                    state[5 * y],
                    state[5 * y + 1],
                    state[5 * y + 2],
                    state[5 * y + 3],
                    state[5 * y + 4],
                ];
                for x in 0..5 {
                    state[5 * y + x] = row[x] ^ (!row[(x + 1) % 5] & row[(x + 2) % 5]);
                }
            }

            // iota
            state[0] ^= RC[round];
        }
    }

    pub fn keccak256(input: &[u8]) -> [u8; 32] {
        let mut state = [0u64; 25];
        let mut offset = 0;

        while input.len() - offset >= RATE {
            absorb(&mut state, &input[offset..offset + RATE]);
            keccak_f(&mut state);
            offset += RATE;
        }

        let mut block = [0u8; RATE];
        let remaining = input.len() - offset;
        block[..remaining].copy_from_slice(&input[offset..]);
        block[remaining] = 0x01;
        block[RATE - 1] |= 0x80;
        absorb(&mut state, &block);
        keccak_f(&mut state);

        let mut out = [0u8; 32];
        for i in 0..4 {
            out[i * 8..(i + 1) * 8].copy_from_slice(&state[i].to_le_bytes());
        }
        out
    }

    fn absorb(state: &mut [u64; 25], block: &[u8]) {
        for (i, chunk) in block.chunks_exact(8).enumerate() {
            let mut word = [0u8; 8];
            word.copy_from_slice(chunk);
            state[i] ^= u64::from_le_bytes(word);
        }
    }
}

pub fn keccak256(input: &[u8]) -> [u8; 32] {
    keccak::keccak256(input)
}

// rlp_push_slice appends a byte string, prefixed with its length.
fn rlp_push_slice(buf: &mut [u8], len: &mut usize, bytes: &[u8]) {
    if bytes.len() == 1 && bytes[0] < 0x80 {
        if *len < buf.len() {
            buf[*len] = bytes[0];
            *len += 1;
        }
        return;
    }
    rlp_push_header(buf, len, bytes.len());
    for b in bytes {
        if *len < buf.len() {
            buf[*len] = *b;
            *len += 1;
        }
    }
}

fn rlp_push_u64(buf: &mut [u8], len: &mut usize, value: u64) {
    rlp_push_uint(buf, len, value as u128);
}

fn rlp_push_u128(buf: &mut [u8], len: &mut usize, value: u128) {
    rlp_push_uint(buf, len, value);
}

fn rlp_push_uint(buf: &mut [u8], len: &mut usize, value: u128) {
    let be = value.to_be_bytes();
    let first = be.iter().position(|&b| b != 0).unwrap_or(be.len());
    let trimmed = &be[first..];
    if trimmed.is_empty() {
        if *len < buf.len() {
            buf[*len] = 0x80;
            *len += 1;
        }
        return;
    }
    if trimmed.len() == 1 && trimmed[0] < 0x80 {
        if *len < buf.len() {
            buf[*len] = trimmed[0];
            *len += 1;
        }
        return;
    }
    rlp_push_header(buf, len, trimmed.len());
    for b in trimmed {
        if *len < buf.len() {
            buf[*len] = *b;
            *len += 1;
        }
    }
}

fn rlp_push_header(buf: &mut [u8], len: &mut usize, size: usize) {
    if size <= 55 {
        if *len < buf.len() {
            buf[*len] = (0x80 + size) as u8;
            *len += 1;
        }
        return;
    }
    let mut be = [0u8; 4];
    let mut v = size;
    let mut n = 0;
    while v > 0 {
        be[n] = (v & 0xff) as u8;
        v >>= 8;
        n += 1;
    }
    let mut first = 0;
    while first < n - 1 && be[first] == 0 {
        first += 1;
    }
    let significant = n - first;
    if *len < buf.len() {
        buf[*len] = (0xb7 + significant) as u8;
        *len += 1;
    }
    for i in 0..significant {
        if *len < buf.len() {
            buf[*len] = be[first + i];
            *len += 1;
        }
    }
}

// selfcheck runs on the first relayed item so a broken hash is reported as a
// refusal rather than silently rejecting every transfer.
pub fn selfcheck() -> bool {
    let empty = keccak256(&[]);
    let expect_empty = [
        0xc5u8, 0xd2, 0x46, 0x01, 0x86, 0xf7, 0x23, 0x3c, 0x92, 0x7e, 0x7d, 0xb2, 0xdc, 0xc7,
        0x03, 0xc0, 0xe5, 0x00, 0xb6, 0x53, 0xca, 0x82, 0x27, 0x3b, 0x7b, 0xfa, 0xd8, 0x04,
        0x5d, 0x85, 0xa4, 0x70,
    ];
    if empty != expect_empty {
        return false;
    }
    // An input past the 136 byte rate exercises the absorb loop and the padding
    // of a trailing partial block.
    let mut long = [0u8; 200];
    for (i, slot) in long.iter_mut().enumerate() {
        *slot = i as u8;
    }
    let long_digest = keccak256(&long);
    let expect_long = [
        0xbfu8, 0xb0, 0xaa, 0x97, 0x86, 0x3e, 0x79, 0x79, 0x43, 0xcf, 0x7c, 0x33, 0xbb, 0x7e,
        0x88, 0x0b, 0xb4, 0x54, 0x3f, 0x3d, 0x27, 0x03, 0xc0, 0x92, 0x3c, 0x69, 0x01, 0xc2,
        0xaf, 0x57, 0xb8, 0x90,
    ];
    long_digest == expect_long
}

// relayed_fields is what a decoded transaction carries once the host has vouched
// for the sender.
pub struct RelayedFields {
    pub from: [u8; 20],
    pub evm_nonce: u64,
    pub to: [u8; 20],
    pub value: u128,
}

// decode_relayed reads a typed EIP-1559 transaction far enough to know where the
// money goes and how much of it there is. The signature is the host's business:
// it verifies with its own implementation and hands over the recovered address,
// because curve arithmetic in a guest without a standard library is a trap this
// program has no reason to take.
pub fn decode_relayed(
    raw: &[u8],
    chain_id: u64,
    recovered: Option<[u8; 20]>,
) -> Result<RelayedFields, &'static str> {
    if raw.len() < 3 || raw[0] != 0x02 {
        return Err("no es una transaccion tipada 0x02");
    }
    let body = &raw[1..];

    let (payload_start, payload_len) = rlp_item(body, 0).ok_or("rlp truncado")?;
    if !rlp_is_list(body, 0) {
        return Err("el cuerpo no es una lista rlp");
    }
    let payload = body
        .get(payload_start..payload_start + payload_len)
        .ok_or("el payload de la lista se sale")?;

    // The recorded offset is where the item starts, header included, because
    // that is what the readers below need in order to decode it.
    let mut fields = Vec::with_capacity(12);
    let mut p = 0;
    while p < payload.len() {
        let (start, len) = rlp_item(payload, p).ok_or("campo rlp truncado")?;
        fields.push((payload_start + p, len));
        p = start + len;
    }
    if fields.len() != 12 {
        return Err("numero de campos inesperado");
    }
    let read = |index: usize| -> Option<(usize, usize)> { fields.get(index).copied() };

    let chain = rlp_uint(body, read(0).ok_or("faltan campos")?.0).ok_or("el id de cadena no cabe")?;
    if chain != chain_id {
        return Err("la cadena no coincide");
    }
    let nonce = rlp_uint(body, read(1).ok_or("faltan campos")?.0).ok_or("el nonce no cabe")?;

    let to_field = read(5).ok_or("faltan campos")?;
    let to_bytes = rlp_bytes(body, to_field.0).ok_or("el destino no se puede leer")?;
    if to_bytes.len() != 20 {
        return Err("el destino no mide 20 bytes");
    }
    let mut to = [0u8; 20];
    to.copy_from_slice(to_bytes);

    let value_bytes = rlp_bytes(body, read(6).ok_or("faltan campos")?.0).ok_or("el valor no se puede leer")?;
    if value_bytes.len() > 16 {
        return Err("el valor no cabe en 128 bits");
    }
    let mut value: u128 = 0;
    for b in value_bytes {
        value = (value << 8) | *b as u128;
    }
    if value == 0 {
        return Err("el valor es cero");
    }

    if recovered.is_none() {
        return Err("el host no verifico la firma");
    }

    Ok(RelayedFields { from: recovered.expect("comprobado antes"), to, value, evm_nonce: nonce })
}

pub fn decode_hex_address(text: &str) -> Option<[u8; 20]> {
    let body = text.trim();
    let body = body.strip_prefix("0x").or_else(|| body.strip_prefix("0X"))?;
    if body.len() != 40 {
        return None;
    }
    let bytes = body.as_bytes();
    let mut out = [0u8; 20];
    let mut i = 0;
    while i < 40 {
        let hi = (bytes[i] as char).to_digit(16)?;
        let lo = (bytes[i + 1] as char).to_digit(16)?;
        out[i / 2] = ((hi << 4) | lo) as u8;
        i += 2;
    }
    Some(out)
}

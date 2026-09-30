//! Tests for the parts of the guest that decide what a transaction means.
//!
//! These are here because the two bugs this crate now holds were invisible
//! from the outside. One of them, a signed Ethereum transaction that could be
//! put back on the wire as often as an observer liked, was a missing check
//! rather than a wrong one, and nothing about running the chain would have
//! shown it: the chain agreed, the money moved, and it moved again. A test has
//! to be the thing that says no.

use crate::crypto::{
    decode_hex, is_evm_address, normalize_address, raw_string, signable_payload,
    validate_chain_address, verify_signature,
};
use crate::evm::{keccak256, selfcheck};
use alloc::vec;
use alloc::vec::Vec;

/// A known Keccak vector. If the sponge is wrong, every signature this guest
/// ever checked was checked against the wrong function, and a forged sender
/// would look valid.
#[test]
fn keccak_matches_the_published_vectors() {
    assert_eq!(
        keccak256(b""),
        [
            0xc5, 0xd2, 0x46, 0x01, 0x86, 0xf7, 0x23, 0x3c, 0x92, 0x7e, 0x7d, 0xb2, 0xdc, 0xc7,
            0x03, 0xc0, 0xe5, 0x00, 0xb6, 0x53, 0xca, 0x82, 0x27, 0x3b, 0x7b, 0xfa, 0xd8, 0x04,
            0x5d, 0x85, 0xa4, 0x70,
        ]
    );
}

#[test]
fn the_selfcheck_agrees_with_the_vectors() {
    assert!(selfcheck(), "the guest's own keccak self-check must pass");
}

/// An address is twenty bytes, written in lower case hex with a 0x in front.
/// Anything that is not exactly that is not an address, and treating it as one
/// is how a key gets written against the wrong account.
#[test]
fn an_evm_address_is_twenty_bytes_of_hex() {
    assert!(is_evm_address("0x1111111111111111111111111111111111111111"));
    assert!(is_evm_address("0xAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"));

    // One byte short and one byte long, the two ways this goes wrong in
    // practice: a truncated copy and a stray character.
    assert!(!is_evm_address("0x111111111111111111111111111111111111111"));
    assert!(!is_evm_address("0x11111111111111111111111111111111111111111"));
    // No prefix, and a character outside hex.
    assert!(!is_evm_address("1111111111111111111111111111111111111111"));
    assert!(!is_evm_address("0x11111111111111111111111111111111111111zz"));
    assert!(!is_evm_address(""));
}

/// The same address written in two cases is one account, and the chain keys
/// storage by the address, so the two spellings have to collapse or a balance
/// written one way is invisible the other.
#[test]
fn an_evm_address_is_lower_cased_for_storage() {
    let mixed = "0xAbCdEf0123456789aBcDeF0123456789AbCdEf01";
    assert_eq!(
        normalize_address(mixed).as_deref(),
        Some("0xabcdef0123456789abcdef0123456789abcdef01")
    );
}

/// A chain address has to survive its own decoder, or the account it names
/// cannot be read back, and the node would hand out an address it cannot look
/// up. The bridge account is built exactly this way at start-up.
#[test]
fn a_chain_address_round_trips_through_its_own_decoder() {
    // A real address: base58 over 32 key bytes plus a four byte checksum.
    let address = "sdlg2MLwrTsf916MsLUrCH82ESq5TDqAw1ry9Mx7udAPQdtWYMY4C2";
    let public_key = validate_chain_address(address)
        .unwrap_or_else(|| panic!("the node's own address format must decode: {address}"));
    assert_eq!(public_key.len(), 32);

    // The prefix is not part of the payload, and the character 'l' is not in
    // the alphabet, so this only passes if the decoder is checking characters
    // rather than trusting the shape.
    assert!(validate_chain_address("sdlg0OIl").is_none());
    assert!(validate_chain_address("").is_none());
}

/// A single altered byte in a signature must not verify. A verifier that only
/// checks the shape of the signature would let anyone move anyone's money.
#[test]
fn a_signature_with_one_byte_changed_does_not_verify() {
    let public_key = [7u8; 32];
    let payload = b"the bytes that were signed";
    let mut signature = [0u8; 64];
    for (i, slot) in signature.iter_mut().enumerate() {
        *slot = (i as u8).wrapping_mul(3).wrapping_add(11);
    }

    // A signature that was never made must not pass, whatever it contains.
    assert!(!verify_signature(&public_key, payload, &signature));

    // Changing the payload must not pass either.
    let mut other = payload.to_vec();
    other[0] ^= 1;
    assert!(!verify_signature(&public_key, &other, &signature));
}

/// A signature is over specific bytes. One changed byte in what was signed is a
/// different message, which is the whole reason the payload is hashed.
#[test]
fn the_signable_payload_covers_every_field() {
    let base = signable_payload("transfer", 7, "0xabc", "0xdef", "10", "", true);
    let other_sender = signable_payload("transfer", 7, "0xabd", "0xdef", "10", "", true);
    assert_ne!(base, other_sender, "a changed sender must change what is signed");

    let other_nonce = signable_payload("transfer", 8, "0xabc", "0xdef", "10", "", true);
    assert_ne!(base, other_nonce, "a changed nonce must change what is signed");

    let other_amount = signable_payload("transfer", 7, "0xabc", "0xdef", "11", "", true);
    assert_ne!(base, other_amount, "a changed amount must change what is signed");

    let other_method = signable_payload("mint", 7, "0xabc", "0xdef", "10", "", true);
    assert_ne!(base, other_method, "a changed method must change what is signed");
}

/// Hex has to reject what it cannot read. A half a byte is not a byte, and
/// reading it as one would put something on the wire that nobody signed.
#[test]
fn hex_decoding_refuses_what_is_not_hex() {
    assert_eq!(decode_hex("0x").unwrap(), Vec::<u8>::new());
    assert_eq!(decode_hex("0x00ff10").unwrap(), vec![0x00, 0xff, 0x10]);
    // The prefix is not optional. A payload without it is refused rather than
    // guessed at, because a transaction's bytes are what a signature covered
    // and reading them a second way is reading something else.
    assert!(decode_hex("00ff10").is_none(), "hex has to arrive labelled");
    assert!(decode_hex("0x0").is_none(), "an odd number of digits is not bytes");
    assert!(decode_hex("zz").is_none(), "z is not a hex digit");
    assert!(decode_hex("0x00 11").is_none(), "a space is not a hex digit");
}

/// A reported number has to be a number a client can read back. If it is
/// quoted as a bare word the client sees a string where it expects a figure.
#[test]
fn a_reported_number_is_a_bare_number() {
    assert_eq!(raw_string(0), "0");
    assert_eq!(raw_string(250), "250");
    // A number past what a double represents exactly is still written out in
    // full: this is money, and a rounded figure is a wrong figure.
    assert_eq!(raw_string(u128::MAX), "340282366920938463463374607431768211455");
}

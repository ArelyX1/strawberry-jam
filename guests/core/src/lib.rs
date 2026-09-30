//! The part of the PAPU guest that has nothing to do with being a guest.
//!
//! Addresses, signatures, Keccak and the decoding of a relayed Ethereum
//! transaction are all ordinary computation: they read a byte slice and answer.
//! None of it needs the PVM, a linker, or a target that can trap, so it lives
//! here, in a crate a test can load, and the binary that has to run inside the
//! PVM depends on it rather than containing it.
//!
//! The split is what makes the economy checkable. When this was all inside the
//! guest binary there was no way to run a test against it, and two of its
//! assumptions turned out to be wrong in ways no amount of reading had caught:
//! the nonce it wrote was sixteen bytes wide and the one it read accepted at
//! most eight, so an account could never move past its first number, and the
//! Ethereum nonce it reported in every report was never checked against
//! anything.

#![no_std]

extern crate alloc;

pub mod crypto;
pub mod evm;

#[cfg(test)]
mod tests;

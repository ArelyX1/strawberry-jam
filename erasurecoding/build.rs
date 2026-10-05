// Exporta las funciones del enlace en Windows, y en ningun otro sitio.
//
// El porque es concreto. Una cdylib en Linux y en macOS deja sus simbolos
// fuera por defecto y Go los encuentra con dlopen. En Windows no: una DLL
// empieza con la tabla de exports vacia y solo aparecen los que alguien declare.
// Y rustc, al compilar para windows-gnu, no declara ninguno, porque da por hecho
// que el enlizador los exporta todos, cosa que hace el ld de GNU con
// --export-all-symbols y no el LLD que usa zig.
//
// El resultado sin esto es una DLL que pesa lo mismo, tiene el aspecto correcto
// y no exporta nada. Se carga sin error, la primera funcion que se busca no
// aparece, y el nodo muere en el arranque con un mensaje que no señala que lo
// que falla es que la libreria esta vacia. Compilar un binario para Windows y
// darlo por bueno porque file dice que es un PE32 no detecta esto.
//
// El fichero .def se genera leyendo los nombres del propio codigo en vez de
// escribirlos aqui. Una lista escrita a mano se queda vieja en cuanto se anade o
// se renombra una funcion, y entonces el fallo aparece en la maquina del
// usuario y no aqui, como una funcion que no existe cuando si existe.

use std::env;
use std::fs;
use std::path::PathBuf;

fn main() {
    println!("cargo:rerun-if-changed=src/lib.rs");
    println!("cargo:rerun-if-changed=build.rs");

    // Solo Windows lo necesita. En Unix exportar de mas no molesta, y tocar el
    // enlace ahi solo aria imposible notar cuando algo se rompe.
    let target_os = env::var("CARGO_CFG_TARGET_OS").unwrap_or_default();
    if target_os != "windows" {
        return;
    }

    let source = fs::read_to_string("src/lib.rs").expect("no se puede leer src/lib.rs");
    let nombres = funciones_exportadas(&source);

    assert!(
        !nombres.is_empty(),
        "no se encontro ninguna funcion #[no_mangle] en src/lib.rs, asi que la DLL \
         no exportaria nada y el nodo no arrancaria en Windows"
    );

    let out_dir = PathBuf::from(env::var("OUT_DIR").expect("falta OUT_DIR"));
    let def = out_dir.join("exports.def");

    let mut contenido = String::from("EXPORTS\n");
    for nombre in &nombres {
        contenido.push_str(nombre);
        contenido.push('\n');
    }
    fs::write(&def, contenido).expect("no se puede escribir el .def");

    // El .def se pasa como un objeto mas del enlace. Es lo que LLD acepta; las
    // opciones --export-all-symbols y --export de GNU no existen para LLD y se
    // ignoran sin avisar.
    println!("cargo:rustc-link-arg-cdylib={}", def.display());
}

/// Devuelve los nombres de las funciones declaradas con `#[no_mangle]` y
/// `extern "C"`, que son las que tienen que salir de la DLL.
fn funciones_exportadas(source: &str) -> Vec<String> {
    let mut nombres = Vec::new();
    let mut pendiente = false;

    for linea in source.lines() {
        let t = linea.trim();

        if t == "#[no_mangle]" || t == "#[unsafe(no_mangle)]" {
            pendiente = true;
            continue;
        }
        if t.starts_with("#[") || t.starts_with("///") || t.starts_with("//") {
            continue;
        }

        if pendiente {
            if let Some(nombre) = nombre_de_funcion(t) {
                nombres.push(nombre);
            }
            pendiente = false;
        }
    }

    nombres.sort();
    nombres.dedup();
    nombres
}

/// Saca el nombre de una linea `pub extern "C" fn nombre(` o
/// `pub unsafe extern "C" fn nombre(`.
fn nombre_de_funcion(linea: &str) -> Option<String> {
    if !linea.contains("extern \"C\"") {
        return None;
    }
    let despues = linea.split("fn ").nth(1)?;
    let nombre: String = despues
        .trim_start()
        .chars()
        .take_while(|c| c.is_ascii_alphanumeric() || *c == '_')
        .collect();
    if nombre.is_empty() {
        None
    } else {
        Some(nombre)
    }
}

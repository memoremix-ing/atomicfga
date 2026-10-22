//! Conditions: a fail-closed subset of CEL as OpenFGA configures cel-go.
//!
//! Supported: parameters of type bool, int, uint, double, string,
//! timestamp, duration and ipaddress; literals of the first five; `!`,
//! unary `-`, `* / %`, `+ -`, comparisons, `&&`, `||`, `?:`; `timestamp`,
//! `duration`, `ipaddress`, `size` and the `startsWith`, `endsWith`,
//! `contains`, `size` and `in_cidr` methods. Expressions are type-checked
//! here with cel-go's overloads (operands of one type), so every accepted
//! expression has a single meaning.
//!
//! Anything else (lists, maps, macros, conversions, timestamp arithmetic,
//! regular expressions, inline `$expression` conditions) is *refused*: the
//! caller learns that this port cannot decide, never a guess. Inputs whose
//! Go parsing this port does not reproduce exactly (unusual timestamp or
//! number spellings, zoned IPv6 addresses) are refused the same way.

use crate::model::{ConditionJson, ParamTypeJson};
use serde_json::{Map, Value};
use std::collections::BTreeMap;
use std::net::IpAddr;

/// A condition result that is not a value.
#[derive(Clone, Debug, PartialEq)]
pub enum Bad {
    /// OpenFGA fails the evaluation (an `ErrEvaluationFailed`, code 2000).
    Error(String),
    /// This port cannot tell what OpenFGA does.
    Refuse(String),
}

pub type Outcome<T> = Result<T, Bad>;

fn err<T>(m: impl Into<String>) -> Outcome<T> {
    Err(Bad::Error(m.into()))
}

fn refuse<T>(m: impl Into<String>) -> Outcome<T> {
    Err(Bad::Refuse(m.into()))
}

#[derive(Clone, Copy, Debug, PartialEq)]
pub enum Ty {
    Bool,
    Int,
    Uint,
    Double,
    String,
    Timestamp,
    Duration,
    Ip,
}

#[derive(Clone, Debug, PartialEq)]
pub enum Val {
    Bool(bool),
    Int(i64),
    Uint(u64),
    Double(f64),
    Str(String),
    /// Seconds since the Unix epoch and nanoseconds: an instant.
    Ts(i64, u32),
    Dur(i64),
    Ip(IpAddr),
}

fn param_type(p: &ParamTypeJson) -> Result<Ty, String> {
    let name = match &p.type_name {
        Value::String(s) => s.as_str(),
        other => return Err(format!("parameter type {other}")),
    };
    if !p.generic_types.is_empty() {
        return Err(format!("generic parameter type {name}"));
    }
    Ok(match name {
        "TYPE_NAME_BOOL" => Ty::Bool,
        "TYPE_NAME_INT" => Ty::Int,
        "TYPE_NAME_UINT" => Ty::Uint,
        "TYPE_NAME_DOUBLE" => Ty::Double,
        "TYPE_NAME_STRING" => Ty::String,
        "TYPE_NAME_TIMESTAMP" => Ty::Timestamp,
        "TYPE_NAME_DURATION" => Ty::Duration,
        "TYPE_NAME_IPADDRESS" => Ty::Ip,
        other => return Err(format!("parameter type {other}")),
    })
}

pub struct Condition {
    pub name: String,
    params: BTreeMap<String, Result<Ty, String>>,
    program: Result<Program, String>,
}

impl Condition {
    pub(crate) fn new(c: &ConditionJson) -> Condition {
        let params: BTreeMap<_, _> = c
            .parameters
            .iter()
            .map(|(k, v)| (k.clone(), param_type(v)))
            .collect();
        let program = (|| {
            let mut types = BTreeMap::new();
            for (k, t) in &params {
                types.insert(k.clone(), t.clone()?);
            }
            Program::compile(&c.expression, &types)
        })();
        Condition {
            name: c.name.clone(),
            params,
            program,
        }
    }

    /// `CastContextToTypedParameters` on a tuple's context, followed by
    /// validateCondition's check for unknown keys. `Error` makes the tuple
    /// invalid.
    pub fn validate_context(&self, ctx: Option<&Map<String, Value>>) -> Outcome<()> {
        let Some(ctx) = ctx.filter(|c| !c.is_empty()) else {
            return Ok(());
        };
        if self.params.is_empty() {
            return err("no parameters defined for the condition");
        }
        let mut refused = None;
        for (k, t) in &self.params {
            if let Some(v) = ctx.get(k) {
                match convert(t, v) {
                    Ok(_) => {}
                    Err(Bad::Error(e)) => return err(e),
                    Err(Bad::Refuse(r)) => refused = Some(r),
                }
            }
        }
        if let Some(k) = ctx.keys().find(|k| !self.params.contains_key(*k)) {
            return err(format!("found invalid context parameter: {k}"));
        }
        match refused {
            Some(r) => refuse(r),
            None => Ok(()),
        }
    }

    /// `EvaluateTupleCondition` for a tuple with this condition.
    pub fn evaluate(
        &self,
        request: Option<&Map<String, Value>>,
        tuple: Option<&Map<String, Value>>,
    ) -> Outcome<bool> {
        let mut merged: BTreeMap<&str, &Value> = BTreeMap::new();
        for m in [request, tuple].into_iter().flatten() {
            for (k, v) in m {
                merged.insert(k, v);
            }
        }
        // Every failure below is code 2000 in OpenFGA whichever comes first,
        // so a certain error outranks a refusal.
        if !merged.is_empty() && self.params.is_empty() {
            return err("no parameters defined for the condition");
        }
        let mut vars = BTreeMap::new();
        let mut refused = None;
        for (k, t) in &self.params {
            if let Some(v) = merged.get(k.as_str()) {
                match convert(t, v) {
                    Ok(v) => {
                        vars.insert(k.as_str(), v);
                    }
                    Err(Bad::Error(e)) => return err(e),
                    Err(Bad::Refuse(r)) => refused = Some(r),
                }
            }
        }
        if let Some(k) = self
            .params
            .keys()
            .find(|k| !merged.contains_key(k.as_str()))
        {
            return err(format!("missing context parameter {k}"));
        }
        if let Some(r) = refused {
            return refuse(r);
        }
        let program = self.program.as_ref().map_err(|r| Bad::Refuse(r.clone()))?;
        program.run(&vars)
    }
}

/// `ParameterType.ConvertValue` of a protobuf Struct value.
fn convert(t: &Result<Ty, String>, v: &Value) -> Outcome<Val> {
    let t = match t {
        Ok(t) => *t,
        Err(r) => return refuse(r.clone()),
    };
    let mismatch = || err(format!("unexpected value {v} for parameter of type {t:?}"));
    match t {
        Ty::Bool => v.as_bool().map(Val::Bool).map_or_else(mismatch, Ok),
        Ty::String => v
            .as_str()
            .map(|s| Val::Str(s.into()))
            .map_or_else(mismatch, Ok),
        Ty::Int | Ty::Uint => {
            // big.Float of the value; Int64() saturates.
            let i = match v {
                Value::Number(n) => {
                    let f = n.as_f64().ok_or_else(|| Bad::Refuse("number".into()))?;
                    if f.fract() != 0.0 || !f.is_finite() {
                        return err(format!("expected an integer, found {f}"));
                    }
                    f as i64
                }
                Value::String(s) => decimal_integer(s)?,
                _ => return mismatch(),
            };
            if t == Ty::Int {
                Ok(Val::Int(i))
            } else if i < 0 {
                err(format!("expected a uint value, found {i}"))
            } else {
                Ok(Val::Uint(i as u64))
            }
        }
        Ty::Double => match v {
            Value::Number(n) => n
                .as_f64()
                .map(Val::Double)
                .ok_or_else(|| Bad::Refuse("number".into())),
            Value::String(s) => {
                let digits = s.strip_prefix(['+', '-']).unwrap_or(s);
                if !digits.is_empty()
                    && digits.len() <= 15
                    && digits.bytes().all(|b| b.is_ascii_digit())
                {
                    Ok(Val::Double(
                        s.parse::<f64>().map_err(|e| Bad::Refuse(e.to_string()))?,
                    ))
                } else {
                    refuse(format!("double spelled {s:?}"))
                }
            }
            _ => mismatch(),
        },
        Ty::Duration => match v {
            Value::String(s) => parse_duration(s).map(Val::Dur).map_err(Bad::Error),
            _ => mismatch(),
        },
        Ty::Timestamp => match v {
            Value::String(s) => parse_timestamp(s),
            _ => mismatch(),
        },
        Ty::Ip => match v {
            Value::String(s) => parse_ip(s).map(Val::Ip),
            _ => mismatch(),
        },
    }
}

/// big.ParseFloat(s, 10, 64, ToNearestEven) followed by Int64, for the
/// spellings it handles exactly: optionally signed decimal digits below 2^64.
fn decimal_integer(s: &str) -> Outcome<i64> {
    let digits = s.strip_prefix(['+', '-']).unwrap_or(s);
    if digits.is_empty() || digits.len() > 19 || !digits.bytes().all(|b| b.is_ascii_digit()) {
        return refuse(format!("integer spelled {s:?}"));
    }
    let n: i128 = s
        .parse()
        .map_err(|_| Bad::Refuse(format!("integer spelled {s:?}")))?;
    Ok(n.clamp(i64::MIN as i128, i64::MAX as i128) as i64)
}

/// Go's time.ParseDuration, ported statement by statement.
pub fn parse_duration(orig: &str) -> Result<i64, String> {
    let invalid = || format!("time: invalid duration {orig:?}");
    let mut s = orig.as_bytes();
    let mut d: u64 = 0;
    let mut neg = false;
    if let Some(&c) = s.first() {
        if c == b'-' || c == b'+' {
            neg = c == b'-';
            s = &s[1..];
        }
    }
    if s == b"0" {
        return Ok(0);
    }
    if s.is_empty() {
        return Err(invalid());
    }
    while !s.is_empty() {
        let mut f: u64 = 0;
        let mut scale: f64 = 1.0;
        if !(s[0] == b'.' || s[0].is_ascii_digit()) {
            return Err(invalid());
        }
        let pl = s.len();
        // leadingInt
        let mut v: u64 = 0;
        let mut i = 0;
        while i < s.len() && s[i].is_ascii_digit() {
            if v > (1u64 << 63) / 10 {
                return Err(invalid());
            }
            v = v * 10 + (s[i] - b'0') as u64;
            if v > 1u64 << 63 {
                return Err(invalid());
            }
            i += 1;
        }
        s = &s[i..];
        let pre = pl != s.len();
        let mut post = false;
        if !s.is_empty() && s[0] == b'.' {
            s = &s[1..];
            let pl = s.len();
            // leadingFraction
            let mut i = 0;
            let mut overflow = false;
            while i < s.len() && s[i].is_ascii_digit() {
                if !overflow {
                    if f > ((1u64 << 63) - 1) / 10 {
                        overflow = true;
                    } else {
                        let y = f * 10 + (s[i] - b'0') as u64;
                        if y > 1u64 << 63 {
                            overflow = true;
                        } else {
                            f = y;
                            scale *= 10.0;
                        }
                    }
                }
                i += 1;
            }
            s = &s[i..];
            post = pl != s.len();
        }
        if !pre && !post {
            return Err(invalid());
        }
        let mut i = 0;
        while i < s.len() && !(s[i] == b'.' || s[i].is_ascii_digit()) {
            i += 1;
        }
        if i == 0 {
            return Err(format!("time: missing unit in duration {orig:?}"));
        }
        let unit: u64 = match &s[..i] {
            b"ns" => 1,
            b"us" => 1_000,
            u if u == "µs".as_bytes() || u == "μs".as_bytes() => 1_000,
            b"ms" => 1_000_000,
            b"s" => 1_000_000_000,
            b"m" => 60_000_000_000,
            b"h" => 3_600_000_000_000,
            _ => return Err(format!("time: unknown unit in duration {orig:?}")),
        };
        s = &s[i..];
        if v > (1u64 << 63) / unit {
            return Err(invalid());
        }
        v *= unit;
        if f > 0 {
            v += (f as f64 * (unit as f64 / scale)) as u64;
            if v > 1u64 << 63 {
                return Err(invalid());
            }
        }
        d += v;
        if d > 1u64 << 63 {
            return Err(invalid());
        }
    }
    if neg {
        return Ok((d as i64).wrapping_neg());
    }
    if d > (1u64 << 63) - 1 {
        return Err(invalid());
    }
    Ok(d as i64)
}

/// RFC 3339 as `time.Parse(time.RFC3339, s)` reads it, for the canonical
/// spelling only: `YYYY-MM-DDTHH:MM:SS[.f{1,9}](Z|±HH:MM)`. Other spellings
/// are refused, as are years 0000 and 9999 (cel-go's timestamp range).
fn parse_timestamp(s: &str) -> Outcome<Val> {
    let no = || refuse(format!("timestamp spelled {s:?}"));
    let b = s.as_bytes();
    let num = |r: std::ops::Range<usize>| -> Option<i64> {
        let p = b.get(r)?;
        p.iter()
            .all(u8::is_ascii_digit)
            .then(|| p.iter().fold(0i64, |a, d| a * 10 + (d - b'0') as i64))
    };
    if b.len() < 20
        || b[4] != b'-'
        || b[7] != b'-'
        || b[10] != b'T'
        || b[13] != b':'
        || b[16] != b':'
    {
        return no();
    }
    let (Some(year), Some(month), Some(day), Some(hour), Some(min), Some(sec)) = (
        num(0..4),
        num(5..7),
        num(8..10),
        num(11..13),
        num(14..16),
        num(17..19),
    ) else {
        return no();
    };
    let mut i = 19;
    let mut nanos = 0u32;
    if b[i] == b'.' {
        let start = i + 1;
        let mut j = start;
        while j < b.len() && b[j].is_ascii_digit() {
            j += 1;
        }
        let digits = j - start;
        if digits == 0 || digits > 9 {
            return no();
        }
        nanos = (num(start..j).unwrap() * 10i64.pow((9 - digits) as u32)) as u32;
        i = j;
    }
    let offset = match &b[i..] {
        b"Z" => 0,
        [sign @ (b'+' | b'-'), _, _, b':', _, _] => {
            let (Some(h), Some(m)) = (num(i + 1..i + 3), num(i + 4..i + 6)) else {
                return no();
            };
            if h >= 24 || m >= 60 {
                return no();
            }
            let o = h * 3600 + m * 60;
            if *sign == b'-' {
                -o
            } else {
                o
            }
        }
        _ => return no(),
    };
    let leap = (year % 4 == 0 && year % 100 != 0) || year % 400 == 0;
    let days_in_month = [
        31,
        if leap { 29 } else { 28 },
        31,
        30,
        31,
        30,
        31,
        31,
        30,
        31,
        30,
        31,
    ];
    if !(1..=9998).contains(&year)
        || !(1..=12).contains(&month)
        || day < 1
        || day > days_in_month[(month - 1) as usize]
        || hour > 23
        || min > 59
        || sec > 59
    {
        return no();
    }
    // Days from civil (Howard Hinnant).
    let y = if month <= 2 { year - 1 } else { year };
    let era = y.div_euclid(400);
    let yoe = y - era * 400;
    let mp = (month + 9) % 12;
    let doy = (153 * mp + 2) / 5 + day - 1;
    let doe = yoe * 365 + yoe / 4 - yoe / 100 + doy;
    let days = era * 146097 + doe - 719468;
    Ok(Val::Ts(
        days * 86400 + hour * 3600 + min * 60 + sec - offset,
        nanos,
    ))
}

/// netip.ParseAddr followed by Unmap. Spellings Rust's parser rejects
/// (zones, for one) are refused rather than reported as errors.
fn parse_ip(s: &str) -> Outcome<IpAddr> {
    let ip: IpAddr = s
        .parse()
        .map_err(|_| Bad::Refuse(format!("IP address spelled {s:?}")))?;
    Ok(unmap(ip))
}

fn unmap(ip: IpAddr) -> IpAddr {
    match ip {
        IpAddr::V6(v6) => v6.to_ipv4_mapped().map_or(ip, IpAddr::V4),
        v4 => v4,
    }
}

/// OpenFGA's `in_cidr`: netip.ParsePrefix, unmapping 4-in-6 prefixes.
fn in_cidr(ip: &IpAddr, cidr: &str) -> Outcome<bool> {
    let no = || refuse(format!("CIDR spelled {cidr:?}"));
    let Some((addr, bits)) = cidr.split_once('/') else {
        return no();
    };
    let Ok(addr) = addr.parse::<IpAddr>() else {
        return no();
    };
    if bits.is_empty()
        || bits.len() > 3
        || !bits.bytes().all(|b| b.is_ascii_digit())
        || (bits.len() > 1 && bits.starts_with('0'))
    {
        return no();
    }
    let mut bits: u32 = bits.parse().unwrap();
    if bits > if addr.is_ipv4() { 32 } else { 128 } {
        return no();
    }
    let mut net = addr;
    if let IpAddr::V6(v6) = addr {
        if let Some(v4) = v6.to_ipv4_mapped() {
            if bits >= 96 {
                net = IpAddr::V4(v4);
                bits -= 96;
            }
        }
    }
    Ok(match (net, ip) {
        (IpAddr::V4(n), IpAddr::V4(a)) => {
            let mask = if bits == 0 {
                0
            } else {
                u32::MAX << (32 - bits)
            };
            u32::from(n) & mask == u32::from(*a) & mask
        }
        (IpAddr::V6(n), IpAddr::V6(a)) => {
            let mask = if bits == 0 {
                0
            } else {
                u128::MAX << (128 - bits)
            };
            u128::from(n) & mask == u128::from(*a) & mask
        }
        _ => false,
    })
}

// ---------------------------------------------------------------------------
// Expressions

#[derive(Clone, Copy, Debug, PartialEq)]
enum Op {
    Eq,
    Ne,
    Lt,
    Le,
    Gt,
    Ge,
    Add,
    Sub,
    Mul,
    Div,
    Mod,
}

#[derive(Clone, Copy, Debug, PartialEq)]
enum Func {
    Timestamp,
    Duration,
    IpAddress,
    Size,
    InCidr,
    StartsWith,
    EndsWith,
    Contains,
}

#[derive(Debug)]
enum Expr {
    Lit(Val),
    Ident(String),
    Not(Box<Expr>),
    Neg(Box<Expr>),
    Bin(Op, Box<Expr>, Box<Expr>),
    And(Box<Expr>, Box<Expr>),
    Or(Box<Expr>, Box<Expr>),
    Cond(Box<Expr>, Box<Expr>, Box<Expr>),
    /// Receiver (for methods) first.
    Call(Func, Vec<Expr>),
}

struct Program {
    expr: Expr,
    nodes: u64,
}

/// cel-go's default runtime cost limit in OpenFGA.
const MAX_COST: u64 = 100;

impl Program {
    /// Parses and type-checks `src`, returning its type.
    fn typed(src: &str, params: &BTreeMap<String, Ty>) -> Result<(Expr, Ty), String> {
        let mut p = Parser {
            src: src.as_bytes(),
            pos: 0,
            depth: 0,
        };
        let expr = p.expr()?;
        p.ws();
        if p.pos != p.src.len() {
            return Err(format!("unsupported CEL at offset {}", p.pos));
        }
        let ty = check(&expr, params)?;
        Ok((expr, ty))
    }

    fn compile(src: &str, params: &BTreeMap<String, Ty>) -> Result<Program, String> {
        let (expr, ty) = Program::typed(src, params)?;
        if ty != Ty::Bool {
            return Err("condition is not a bool expression".into());
        }
        let nodes = count(&expr);
        Ok(Program { expr, nodes })
    }

    fn run(&self, vars: &BTreeMap<&str, Val>) -> Outcome<bool> {
        let mut cost = self.nodes;
        let v = eval(&self.expr, vars, &mut cost)?;
        if cost > MAX_COST {
            // An upper bound of cel-go's cost; it may or may not abort.
            return refuse("condition may exceed the evaluation cost limit");
        }
        match v {
            Val::Bool(b) => Ok(b),
            _ => refuse("condition did not yield a bool"),
        }
    }
}

fn count(e: &Expr) -> u64 {
    1 + match e {
        Expr::Lit(_) | Expr::Ident(_) => 0,
        Expr::Not(a) | Expr::Neg(a) => count(a),
        Expr::Bin(_, a, b) | Expr::And(a, b) | Expr::Or(a, b) => count(a) + count(b),
        Expr::Cond(a, b, c) => count(a) + count(b) + count(c),
        Expr::Call(_, args) => args.iter().map(count).sum(),
    }
}

fn check(e: &Expr, params: &BTreeMap<String, Ty>) -> Result<Ty, String> {
    use Ty::*;
    let bad = |what: std::string::String| Err(format!("unsupported CEL: {what}"));
    Ok(match e {
        Expr::Lit(v) => match v {
            Val::Bool(_) => Bool,
            Val::Int(_) => Int,
            Val::Uint(_) => Uint,
            Val::Double(_) => Double,
            Val::Str(_) => String,
            _ => return bad("literal".into()),
        },
        Expr::Ident(n) => match params.get(n) {
            Some(t) => *t,
            None => return bad(format!("undeclared identifier {n}")),
        },
        Expr::Not(a) => match check(a, params)? {
            Bool => Bool,
            t => return bad(format!("!{t:?}")),
        },
        Expr::Neg(a) => match check(a, params)? {
            t @ (Int | Double) => t,
            t => return bad(format!("-{t:?}")),
        },
        Expr::And(a, b) | Expr::Or(a, b) => match (check(a, params)?, check(b, params)?) {
            (Bool, Bool) => Bool,
            (x, y) => return bad(format!("logic on {x:?}, {y:?}")),
        },
        Expr::Cond(c, a, b) => match (check(c, params)?, check(a, params)?, check(b, params)?) {
            (Bool, x, y) if x == y => x,
            (c, x, y) => return bad(format!("{c:?} ? {x:?} : {y:?}")),
        },
        Expr::Bin(op, a, b) => {
            let (x, y) = (check(a, params)?, check(b, params)?);
            if x != y {
                return bad(format!("{op:?} on {x:?}, {y:?}"));
            }
            match (op, x) {
                (Op::Eq | Op::Ne, _) => Bool,
                (
                    Op::Lt | Op::Le | Op::Gt | Op::Ge,
                    Bool | Int | Uint | Double | String | Timestamp | Duration,
                ) => Bool,
                (Op::Add, Int | Uint | Double | String) => x,
                (Op::Sub | Op::Mul | Op::Div, Int | Uint | Double) => x,
                (Op::Mod, Int | Uint) => x,
                _ => return bad(format!("{op:?} on {x:?}")),
            }
        }
        Expr::Call(f, args) => {
            let types = args
                .iter()
                .map(|a| check(a, params))
                .collect::<Result<Vec<_>, _>>()?;
            match (f, types.as_slice()) {
                (Func::Timestamp, [String]) => Timestamp,
                (Func::Duration, [String]) => Duration,
                (Func::IpAddress, [String]) => Ip,
                (Func::Size, [String]) => Int,
                (Func::InCidr, [Ip, String]) => Bool,
                (Func::StartsWith | Func::EndsWith | Func::Contains, [String, String]) => Bool,
                _ => return bad(format!("{f:?}{types:?}")),
            }
        }
    })
}

/// Joins two operand results of a strict function: an error is certain
/// (cel-go propagates errors from arguments), a refusal is not.
fn both(a: Outcome<Val>, b: Outcome<Val>) -> Outcome<(Val, Val)> {
    match (a, b) {
        (Err(Bad::Error(e)), _) | (_, Err(Bad::Error(e))) => err(e),
        (Err(r), _) | (_, Err(r)) => Err(r),
        (Ok(a), Ok(b)) => Ok((a, b)),
    }
}

fn str_cost(n: usize) -> u64 {
    (n as u64).div_ceil(10)
}

fn finite(f: f64) -> Outcome<Val> {
    if f.is_nan() {
        refuse("NaN")
    } else {
        Ok(Val::Double(f))
    }
}

fn eval(e: &Expr, vars: &BTreeMap<&str, Val>, cost: &mut u64) -> Outcome<Val> {
    match e {
        Expr::Lit(v) => Ok(v.clone()),
        Expr::Ident(n) => vars
            .get(n.as_str())
            .cloned()
            .ok_or_else(|| Bad::Refuse(format!("unbound {n}"))),
        Expr::Not(a) => match eval(a, vars, cost)? {
            Val::Bool(b) => Ok(Val::Bool(!b)),
            _ => refuse("type"),
        },
        Expr::Neg(a) => match eval(a, vars, cost)? {
            Val::Int(i) => i
                .checked_neg()
                .map(Val::Int)
                .map_or_else(|| err("integer overflow"), Ok),
            Val::Double(f) => finite(-f),
            _ => refuse("type"),
        },
        Expr::And(a, b) | Expr::Or(a, b) => {
            let is_and = matches!(e, Expr::And(..));
            let (x, y) = (eval(a, vars, cost), eval(b, vars, cost));
            // cel-go's commutative logic: the deciding value absorbs errors.
            let decides = |r: &Outcome<Val>| matches!(r, Ok(Val::Bool(v)) if *v != is_and);
            if decides(&x) || decides(&y) {
                return Ok(Val::Bool(!is_and));
            }
            for r in [&x, &y] {
                if let Err(Bad::Refuse(m)) = r {
                    return refuse(m.clone());
                }
            }
            for r in [&x, &y] {
                if let Err(Bad::Error(m)) = r {
                    return err(m.clone());
                }
            }
            Ok(Val::Bool(is_and))
        }
        Expr::Cond(c, a, b) => match eval(c, vars, cost)? {
            Val::Bool(true) => eval(a, vars, cost),
            Val::Bool(false) => eval(b, vars, cost),
            _ => refuse("type"),
        },
        Expr::Bin(op, a, b) => {
            let (x, y) = both(eval(a, vars, cost), eval(b, vars, cost))?;
            binary(*op, x, y, cost)
        }
        Expr::Call(f, args) => {
            let mut vals = Vec::with_capacity(args.len());
            let mut refused = None;
            for a in args {
                match eval(a, vars, cost) {
                    Ok(v) => vals.push(v),
                    Err(Bad::Error(e)) => return err(e),
                    Err(Bad::Refuse(r)) => refused = Some(r),
                }
            }
            if let Some(r) = refused {
                return refuse(r);
            }
            call(*f, vals, cost)
        }
    }
}

fn binary(op: Op, x: Val, y: Val, cost: &mut u64) -> Outcome<Val> {
    use std::cmp::Ordering;
    use Val::*;
    let ord: Option<Ordering> = match (&x, &y) {
        (Bool(a), Bool(b)) => Some(a.cmp(b)),
        (Int(a), Int(b)) => Some(a.cmp(b)),
        (Uint(a), Uint(b)) => Some(a.cmp(b)),
        (Double(a), Double(b)) => a.partial_cmp(b),
        (Str(a), Str(b)) => {
            *cost += str_cost(a.len().max(b.len()));
            Some(a.as_bytes().cmp(b.as_bytes()))
        }
        (Ts(s1, n1), Ts(s2, n2)) => Some((s1, n1).cmp(&(s2, n2))),
        (Dur(a), Dur(b)) => Some(a.cmp(b)),
        (Ip(a), Ip(b)) => Some(if a == b {
            Ordering::Equal
        } else {
            Ordering::Less
        }),
        _ => return refuse("type"),
    };
    let overflow = || err("integer overflow");
    match op {
        Op::Eq | Op::Ne => {
            let eq = match (&x, &y) {
                (Double(a), Double(b)) => a == b,
                _ => ord == Some(Ordering::Equal),
            };
            Ok(Bool(eq == (op == Op::Eq)))
        }
        Op::Lt | Op::Le | Op::Gt | Op::Ge => {
            if matches!(x, Ip(_)) {
                return refuse("IP ordering");
            }
            let Some(o) = ord else {
                return refuse("NaN ordering");
            };
            Ok(Bool(match op {
                Op::Lt => o == Ordering::Less,
                Op::Le => o != Ordering::Greater,
                Op::Gt => o == Ordering::Greater,
                _ => o != Ordering::Less,
            }))
        }
        _ => match (op, x, y) {
            (Op::Add, Int(a), Int(b)) => a.checked_add(b).map(Int).map_or_else(overflow, Ok),
            (Op::Sub, Int(a), Int(b)) => a.checked_sub(b).map(Int).map_or_else(overflow, Ok),
            (Op::Mul, Int(a), Int(b)) => a.checked_mul(b).map(Int).map_or_else(overflow, Ok),
            (Op::Div, Int(_), Int(0)) => err("divide by zero"),
            (Op::Div, Int(a), Int(b)) => a.checked_div(b).map(Int).map_or_else(overflow, Ok),
            (Op::Mod, Int(_), Int(0)) => err("modulus by zero"),
            (Op::Mod, Int(a), Int(b)) => a.checked_rem(b).map(Int).map_or_else(overflow, Ok),
            (Op::Add, Uint(a), Uint(b)) => a.checked_add(b).map(Uint).map_or_else(overflow, Ok),
            (Op::Sub, Uint(a), Uint(b)) => a.checked_sub(b).map(Uint).map_or_else(overflow, Ok),
            (Op::Mul, Uint(a), Uint(b)) => a.checked_mul(b).map(Uint).map_or_else(overflow, Ok),
            (Op::Div, Uint(_), Uint(0)) => err("divide by zero"),
            (Op::Div, Uint(a), Uint(b)) => Ok(Uint(a / b)),
            (Op::Mod, Uint(_), Uint(0)) => err("modulus by zero"),
            (Op::Mod, Uint(a), Uint(b)) => Ok(Uint(a % b)),
            (Op::Add, Double(a), Double(b)) => finite(a + b),
            (Op::Sub, Double(a), Double(b)) => finite(a - b),
            (Op::Mul, Double(a), Double(b)) => finite(a * b),
            (Op::Div, Double(a), Double(b)) => finite(a / b),
            (Op::Add, Str(a), Str(b)) => {
                *cost += str_cost(a.len() + b.len());
                Ok(Str(a + &b))
            }
            _ => refuse("operation"),
        },
    }
}

fn call(f: Func, args: Vec<Val>, cost: &mut u64) -> Outcome<Val> {
    use Val::*;
    match (f, args.as_slice()) {
        (Func::Timestamp, [Str(s)]) => parse_timestamp(s),
        (Func::Duration, [Str(s)]) => parse_duration(s).map(Dur).map_err(Bad::Error),
        (Func::IpAddress, [Str(s)]) => parse_ip(s).map(Ip),
        (Func::Size, [Str(s)]) => Ok(Int(s.chars().count() as i64)),
        (Func::InCidr, [Ip(ip), Str(c)]) => in_cidr(ip, c).map(Bool),
        (Func::StartsWith, [Str(a), Str(b)]) => {
            *cost += str_cost(b.len());
            Ok(Bool(a.starts_with(b.as_str())))
        }
        (Func::EndsWith, [Str(a), Str(b)]) => {
            *cost += str_cost(b.len());
            Ok(Bool(a.ends_with(b.as_str())))
        }
        (Func::Contains, [Str(a), Str(b)]) => {
            *cost += str_cost(a.len()) * str_cost(b.len());
            Ok(Bool(a.contains(b.as_str())))
        }
        _ => refuse("call"),
    }
}

/// Whether OpenFGA accepts a condition when a model is written
/// (`EvaluableCondition.Compile`).
#[derive(Clone, Debug, PartialEq)]
pub enum Verdict {
    Valid,
    Invalid(String),
    /// Outside the subset this port decides.
    Undecided(String),
}

/// `types.DecodeParameterType`, recursively; `Err` fails compilation.
fn decode_param(p: &ParamTypeJson) -> Result<Option<Ty>, String> {
    let name = match &p.type_name {
        Value::String(s) => s.as_str(),
        Value::Null => "TYPE_NAME_UNSPECIFIED",
        other => return Err(format!("parameter type {other}")),
    };
    let (ty, generics) = match name {
        "TYPE_NAME_BOOL" => (Some(Ty::Bool), 0),
        "TYPE_NAME_INT" => (Some(Ty::Int), 0),
        "TYPE_NAME_UINT" => (Some(Ty::Uint), 0),
        "TYPE_NAME_DOUBLE" => (Some(Ty::Double), 0),
        "TYPE_NAME_STRING" => (Some(Ty::String), 0),
        "TYPE_NAME_TIMESTAMP" => (Some(Ty::Timestamp), 0),
        "TYPE_NAME_DURATION" => (Some(Ty::Duration), 0),
        "TYPE_NAME_IPADDRESS" => (Some(Ty::Ip), 0),
        "TYPE_NAME_ANY" => (None, 0),
        "TYPE_NAME_LIST" | "TYPE_NAME_MAP" => (None, 1),
        other => return Err(format!("unknown condition parameter type {other}")),
    };
    if p.generic_types.len() != generics {
        return Err(format!(
            "type {name} requires {generics} generic types; found {}",
            p.generic_types.len()
        ));
    }
    for g in &p.generic_types {
        decode_param(g)?;
    }
    Ok(ty)
}

const CEL_RESERVED: &[&str] = &[
    "as",
    "break",
    "const",
    "continue",
    "else",
    "false",
    "for",
    "function",
    "if",
    "import",
    "in",
    "let",
    "loop",
    "package",
    "namespace",
    "null",
    "return",
    "true",
    "var",
    "void",
    "while",
    "int",
    "uint",
    "double",
    "bool",
    "string",
    "bytes",
    "list",
    "map",
    "null_type",
    "type",
    "dyn",
    "timestamp",
    "duration",
    "ipaddress",
    "size",
    "has",
    "all",
    "exists",
    "exists_one",
    "filter",
    "matches",
    "in_cidr",
];

fn plain_identifier(s: &str) -> bool {
    let mut cs = s.chars();
    matches!(cs.next(), Some(c) if c.is_ascii_alphabetic() || c == '_')
        && cs.all(|c| c.is_ascii_alphanumeric() || c == '_')
        && !CEL_RESERVED.contains(&s)
}

/// Whether cel-go, as OpenFGA configures it, compiles the condition: its
/// parameter types decode, and the expression type-checks with a bool
/// result. Expressions outside the supported subset are undecided, as are
/// constant subexpressions this port cannot fold exactly as cel-go would.
pub(crate) fn compile_verdict(c: &ConditionJson) -> Verdict {
    let mut types = BTreeMap::new();
    let mut opaque = false;
    for (name, p) in &c.parameters {
        match decode_param(p) {
            Err(e) => {
                return Verdict::Invalid(format!(
                    "failed to decode parameter type for parameter '{name}': {e}"
                ))
            }
            Ok(Some(t)) => {
                types.insert(name.clone(), t);
            }
            Ok(None) => opaque = true,
        }
    }
    if c.expression.trim().is_empty() {
        return Verdict::Invalid("empty expression".into());
    }
    if let Some(name) = c.parameters.keys().find(|k| !plain_identifier(k)) {
        // How cel-go declares such a variable is not modelled here.
        return Verdict::Undecided(format!("parameter name {name:?}"));
    }
    let undecided = |why: String| {
        let note = if opaque {
            " (with parameters of unsupported types)"
        } else {
            ""
        };
        Verdict::Undecided(format!("{why}{note}"))
    };
    match Program::typed(&c.expression, &types) {
        Err(e) => match e.strip_prefix("unsupported CEL: undeclared identifier ") {
            // Not a parameter of any type, nor a name CEL predeclares.
            Some(name) if plain_identifier(name) && !c.parameters.contains_key(name) => {
                Verdict::Invalid(format!("undeclared reference to '{name}'"))
            }
            _ => undecided(e),
        },
        Ok((_, ty)) if ty != Ty::Bool => Verdict::Invalid(format!(
            "expected a bool condition expression output, but got '{ty:?}'"
        )),
        Ok((expr, _)) => match constant_ok(&expr) {
            Ok(_) => Verdict::Valid,
            Err(e) => undecided(e),
        },
    }
}

/// cel-go folds constant subexpressions when the program is planned; one
/// that fails (`1 / 0`, `timestamp("x")`) may fail compilation. Accept only
/// constants this port evaluates cleanly.
fn constant_ok(e: &Expr) -> Result<bool, String> {
    let children: Vec<&Expr> = match e {
        Expr::Lit(_) => return Ok(true),
        Expr::Ident(_) => return Ok(false),
        Expr::Not(a) | Expr::Neg(a) => vec![a],
        Expr::Bin(_, a, b) | Expr::And(a, b) | Expr::Or(a, b) => vec![a, b],
        Expr::Cond(a, b, c) => vec![a, b, c],
        Expr::Call(_, args) => args.iter().collect(),
    };
    let mut constant = true;
    for c in children {
        constant &= constant_ok(c)?;
    }
    if constant {
        let mut cost = 0;
        if let Err(b) = eval(e, &BTreeMap::new(), &mut cost) {
            return Err(format!(
                "constant subexpression does not fold cleanly: {b:?}"
            ));
        }
    }
    Ok(constant)
}

// ---------------------------------------------------------------------------
// Parser (CEL grammar, restricted to the supported subset)

struct Parser<'a> {
    src: &'a [u8],
    pos: usize,
    depth: usize,
}

type PResult = Result<Expr, String>;

impl Parser<'_> {
    fn ws(&mut self) {
        loop {
            while self.pos < self.src.len()
                && matches!(self.src[self.pos], b' ' | b'\t' | b'\n' | b'\r' | b'\x0c')
            {
                self.pos += 1;
            }
            if self.src[self.pos..].starts_with(b"//") {
                while self.pos < self.src.len() && self.src[self.pos] != b'\n' {
                    self.pos += 1;
                }
                continue;
            }
            return;
        }
    }

    fn eat(&mut self, tok: &str) -> bool {
        self.ws();
        if self.src[self.pos..].starts_with(tok.as_bytes()) {
            self.pos += tok.len();
            true
        } else {
            false
        }
    }

    fn peek(&mut self) -> Option<u8> {
        self.ws();
        self.src.get(self.pos).copied()
    }

    fn unsupported<T>(&self, what: &str) -> Result<T, String> {
        Err(format!("unsupported CEL {what} at offset {}", self.pos))
    }

    fn expr(&mut self) -> PResult {
        self.depth += 1;
        if self.depth > 64 {
            return self.unsupported("nesting");
        }
        let c = self.or()?;
        let r = if self.eat("?") {
            let a = self.or()?;
            if !self.eat(":") {
                return self.unsupported("conditional");
            }
            let b = self.expr()?;
            Expr::Cond(Box::new(c), Box::new(a), Box::new(b))
        } else {
            c
        };
        self.depth -= 1;
        Ok(r)
    }

    fn or(&mut self) -> PResult {
        let mut e = self.and()?;
        while self.eat("||") {
            e = Expr::Or(Box::new(e), Box::new(self.and()?));
        }
        Ok(e)
    }

    fn and(&mut self) -> PResult {
        let mut e = self.relation()?;
        while self.eat("&&") {
            e = Expr::And(Box::new(e), Box::new(self.relation()?));
        }
        Ok(e)
    }

    fn relation(&mut self) -> PResult {
        let mut e = self.addition()?;
        loop {
            let op = if self.eat("==") {
                Op::Eq
            } else if self.eat("!=") {
                Op::Ne
            } else if self.eat("<=") {
                Op::Le
            } else if self.eat(">=") {
                Op::Ge
            } else if self.eat("<") {
                Op::Lt
            } else if self.eat(">") {
                Op::Gt
            } else {
                return Ok(e);
            };
            e = Expr::Bin(op, Box::new(e), Box::new(self.addition()?));
        }
    }

    fn addition(&mut self) -> PResult {
        let mut e = self.multiplication()?;
        loop {
            let op = if self.eat("+") {
                Op::Add
            } else if self.peek() == Some(b'-') {
                self.pos += 1;
                Op::Sub
            } else {
                return Ok(e);
            };
            e = Expr::Bin(op, Box::new(e), Box::new(self.multiplication()?));
        }
    }

    fn multiplication(&mut self) -> PResult {
        let mut e = self.unary()?;
        loop {
            let op = if self.eat("*") {
                Op::Mul
            } else if self.peek() == Some(b'/') && self.src.get(self.pos + 1) != Some(&b'/') {
                self.pos += 1;
                Op::Div
            } else if self.eat("%") {
                Op::Mod
            } else {
                return Ok(e);
            };
            e = Expr::Bin(op, Box::new(e), Box::new(self.unary()?));
        }
    }

    fn unary(&mut self) -> PResult {
        if self.peek() == Some(b'!') && self.src.get(self.pos + 1) != Some(&b'=') {
            self.pos += 1;
            if matches!(self.peek(), Some(b'!' | b'-')) {
                return self.unsupported("repeated unary operator");
            }
            return Ok(Expr::Not(Box::new(self.member()?)));
        }
        if self.peek() == Some(b'-') {
            self.pos += 1;
            if matches!(self.peek(), Some(b'!' | b'-')) {
                return self.unsupported("repeated unary operator");
            }
            if self.peek().is_some_and(|c| c.is_ascii_digit() || c == b'.') {
                // cel-go folds `-` into a numeric literal (so -2^63 is valid).
                let lit = self.number(true)?;
                return self.member_tail(lit);
            }
            return Ok(Expr::Neg(Box::new(self.member()?)));
        }
        self.member()
    }

    fn member(&mut self) -> PResult {
        let p = self.primary()?;
        self.member_tail(p)
    }

    fn member_tail(&mut self, mut e: Expr) -> PResult {
        loop {
            match self.peek() {
                Some(b'.') => {
                    self.pos += 1;
                    let name = self.ident().ok_or("expected a member name")?;
                    if !self.eat("(") {
                        return self.unsupported("field selection");
                    }
                    let f = match name.as_str() {
                        "startsWith" => Func::StartsWith,
                        "endsWith" => Func::EndsWith,
                        "contains" => Func::Contains,
                        "size" => Func::Size,
                        "in_cidr" => Func::InCidr,
                        _ => return self.unsupported(&format!("method {name}")),
                    };
                    let mut args = vec![e];
                    args.extend(self.args()?);
                    e = Expr::Call(f, args);
                }
                Some(b'[') => return self.unsupported("indexing"),
                _ => return Ok(e),
            }
        }
    }

    fn args(&mut self) -> Result<Vec<Expr>, String> {
        let mut args = vec![];
        if self.eat(")") {
            return Ok(args);
        }
        loop {
            args.push(self.expr()?);
            if self.eat(")") {
                return Ok(args);
            }
            if !self.eat(",") {
                return self.unsupported("argument list");
            }
        }
    }

    fn ident(&mut self) -> Option<String> {
        self.ws();
        let start = self.pos;
        while self.pos < self.src.len()
            && (self.src[self.pos].is_ascii_alphanumeric() || self.src[self.pos] == b'_')
        {
            if self.pos == start && self.src[self.pos].is_ascii_digit() {
                return None;
            }
            self.pos += 1;
        }
        (self.pos > start).then(|| String::from_utf8_lossy(&self.src[start..self.pos]).into_owned())
    }

    fn primary(&mut self) -> PResult {
        match self.peek() {
            Some(b'(') => {
                self.pos += 1;
                let e = self.expr()?;
                if !self.eat(")") {
                    return self.unsupported("parenthesis");
                }
                Ok(e)
            }
            Some(b'"' | b'\'') => self.string(),
            Some(c) if c.is_ascii_digit() => self.number(false),
            Some(c) if c.is_ascii_alphabetic() || c == b'_' => {
                let name = self.ident().unwrap();
                if self.peek() == Some(b'"') || self.peek() == Some(b'\'') {
                    return self.unsupported("string prefix");
                }
                match name.as_str() {
                    "true" => return Ok(Expr::Lit(Val::Bool(true))),
                    "false" => return Ok(Expr::Lit(Val::Bool(false))),
                    "null" | "in" | "as" | "break" | "const" | "continue" | "else" | "for"
                    | "function" | "if" | "import" | "let" | "loop" | "package" | "namespace"
                    | "return" | "var" | "void" | "while" => return self.unsupported(&name),
                    _ => {}
                }
                if self.eat("(") {
                    let f = match name.as_str() {
                        "timestamp" => Func::Timestamp,
                        "duration" => Func::Duration,
                        "ipaddress" => Func::IpAddress,
                        "size" => Func::Size,
                        _ => return self.unsupported(&format!("function {name}")),
                    };
                    return Ok(Expr::Call(f, self.args()?));
                }
                Ok(Expr::Ident(name))
            }
            _ => self.unsupported("expression"),
        }
    }

    fn number(&mut self, negative: bool) -> PResult {
        let start = self.pos;
        let s = self.src;
        let mut i = self.pos;
        while i < s.len() && s[i].is_ascii_digit() {
            i += 1;
        }
        let int_digits = i - start;
        let mut float = false;
        if i < s.len() && s[i] == b'.' && s.get(i + 1).is_some_and(u8::is_ascii_digit) {
            float = true;
            i += 1;
            while i < s.len() && s[i].is_ascii_digit() {
                i += 1;
            }
        }
        if i < s.len() && (s[i] == b'e' || s[i] == b'E') {
            float = true;
            i += 1;
            if i < s.len() && (s[i] == b'+' || s[i] == b'-') {
                i += 1;
            }
            let d = i;
            while i < s.len() && s[i].is_ascii_digit() {
                i += 1;
            }
            if i == d {
                return self.unsupported("exponent");
            }
        }
        let text = std::str::from_utf8(&s[start..i]).unwrap();
        if !float && int_digits > 1 && text.starts_with('0') {
            // 0x… hex, or a leading zero (strconv base 0 reads octal).
            return self.unsupported("integer spelling");
        }
        if int_digits == 0 && !float {
            return self.unsupported("number");
        }
        let unsigned = i < s.len() && (s[i] == b'u' || s[i] == b'U');
        if i < s.len() && (s[i].is_ascii_alphanumeric() || s[i] == b'_') && !unsigned {
            return self.unsupported("number suffix");
        }
        let signed = if negative {
            format!("-{text}")
        } else {
            text.to_string()
        };
        let lit = if float {
            if unsigned {
                return self.unsupported("number suffix");
            }
            Val::Double(signed.parse::<f64>().map_err(|e| e.to_string())?)
        } else if unsigned {
            i += 1;
            if negative {
                return self.unsupported("negative uint");
            }
            Val::Uint(
                text.parse::<u64>()
                    .map_err(|_| "uint literal out of range".to_string())?,
            )
        } else {
            Val::Int(
                signed
                    .parse::<i64>()
                    .map_err(|_| "int literal out of range".to_string())?,
            )
        };
        self.pos = i;
        Ok(Expr::Lit(lit))
    }

    fn string(&mut self) -> PResult {
        let q = self.src[self.pos];
        if self.src[self.pos..].starts_with(&[q, q, q]) {
            return self.unsupported("triple-quoted string");
        }
        self.pos += 1;
        let mut out = Vec::new();
        loop {
            let Some(&c) = self.src.get(self.pos) else {
                return self.unsupported("unterminated string");
            };
            self.pos += 1;
            match c {
                _ if c == q => break,
                b'\n' | b'\r' => return self.unsupported("newline in string"),
                b'\\' => {
                    let Some(&e) = self.src.get(self.pos) else {
                        return self.unsupported("escape");
                    };
                    self.pos += 1;
                    out.push(match e {
                        b'\\' => b'\\',
                        b'\'' => b'\'',
                        b'"' => b'"',
                        b'n' => b'\n',
                        b't' => b'\t',
                        b'r' => b'\r',
                        _ => return self.unsupported("escape"),
                    });
                }
                _ => out.push(c),
            }
        }
        let s = String::from_utf8(out).map_err(|_| "string literal is not UTF-8".to_string())?;
        Ok(Expr::Lit(Val::Str(s)))
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use serde_json::json;

    fn cond(expr: &str, params: &[(&str, &str)]) -> Condition {
        let c: ConditionJson = serde_json::from_value(json!({
            "name": "c",
            "expression": expr,
            "parameters": params.iter().map(|(k, t)| (k.to_string(), json!({"type_name": format!("TYPE_NAME_{}", t.to_uppercase())}))).collect::<Map<_, _>>(),
        }))
        .unwrap();
        Condition::new(&c)
    }

    fn eval(c: &Condition, ctx: Value) -> Outcome<bool> {
        c.evaluate(ctx.as_object(), None)
    }

    #[test]
    fn upstream_conditions() {
        let c = cond("x < 100 || y < 50", &[("x", "int"), ("y", "int")]);
        assert_eq!(eval(&c, json!({"x": 101, "y": 1})), Ok(true));
        assert_eq!(eval(&c, json!({"x": 101, "y": 51})), Ok(false));
        assert!(
            matches!(eval(&c, json!({"x": 1})), Err(Bad::Error(_))),
            "missing parameter"
        );
        assert!(matches!(
            eval(&c, json!({"x": 1.5, "y": 1})),
            Err(Bad::Error(_))
        ));
        assert_eq!(eval(&c, json!({"x": "99", "y": 99})), Ok(true));

        let c = cond(
            r#"ts < timestamp("2023-10-11T10:00:00.000Z")"#,
            &[("ts", "timestamp")],
        );
        assert_eq!(eval(&c, json!({"ts": "2023-10-11T09:59:59Z"})), Ok(true));
        assert_eq!(
            eval(&c, json!({"ts": "2023-10-11T11:00:00+01:00"})),
            Ok(false)
        );
        assert!(matches!(eval(&c, json!({"ts": 5})), Err(Bad::Error(_))));

        let c = cond(
            "user_ip.in_cidr(cidr)",
            &[("user_ip", "ipaddress"), ("cidr", "string")],
        );
        assert_eq!(
            eval(
                &c,
                json!({"user_ip": "192.168.0.1", "cidr": "192.168.0.0/24"})
            ),
            Ok(true)
        );
        assert_eq!(
            eval(
                &c,
                json!({"user_ip": "::ffff:192.168.0.1", "cidr": "::ffff:192.168.0.0/120"})
            ),
            Ok(true)
        );
        assert_eq!(
            eval(&c, json!({"user_ip": "10.0.0.1", "cidr": "192.168.0.0/24"})),
            Ok(false)
        );

        let c = cond("x > 0.0", &[("x", "double")]);
        assert_eq!(eval(&c, json!({"x": 0.5})), Ok(true));
        let c = cond("x == '1'", &[("x", "string")]);
        assert_eq!(eval(&c, json!({"x": "1"})), Ok(true));
    }

    #[test]
    fn errors_and_refusals() {
        let c = cond("x + 1 > 0 || ok", &[("x", "int"), ("ok", "bool")]);
        assert_eq!(
            eval(&c, json!({"x": i64::MAX as f64, "ok": true})),
            Ok(true),
            "true absorbs the overflow"
        );
        assert!(matches!(
            eval(&c, json!({"x": i64::MAX as f64, "ok": false})),
            Err(Bad::Error(_))
        ));
        let c = cond("x / y == 0", &[("x", "int"), ("y", "int")]);
        assert!(matches!(
            eval(&c, json!({"x": 1, "y": 0})),
            Err(Bad::Error(_))
        ));
        assert!(matches!(
            cond("x in [1]", &[("x", "int")]).evaluate(json!({"x": 1}).as_object(), None),
            Err(Bad::Refuse(_))
        ));
        assert!(matches!(
            cond("x == 1.0", &[("x", "int")]).evaluate(json!({"x": 1}).as_object(), None),
            Err(Bad::Refuse(_))
        ));
        let c = cond("true", &[]);
        assert_eq!(c.evaluate(None, None), Ok(true));
        assert!(matches!(
            c.evaluate(json!({"a": 1}).as_object(), None),
            Err(Bad::Error(_))
        ));
        let c = cond("x == -9223372036854775808", &[("x", "int")]);
        assert_eq!(eval(&c, json!({"x": -9223372036854775808.0})), Ok(true));
    }

    #[test]
    fn durations_match_go() {
        assert_eq!(parse_duration("1h30m"), Ok(5_400_000_000_000));
        assert_eq!(parse_duration("-1.5h"), Ok(-5_400_000_000_000));
        assert_eq!(parse_duration("300ms"), Ok(300_000_000));
        assert_eq!(parse_duration("1µs"), Ok(1_000));
        assert_eq!(parse_duration(".5s"), Ok(500_000_000));
        assert_eq!(parse_duration("0"), Ok(0));
        assert!(parse_duration("").is_err());
        assert!(parse_duration("1").is_err());
        assert!(parse_duration("1d").is_err());
        assert!(parse_duration("9223372036854775808ns").is_err());
        assert_eq!(parse_duration("-9223372036854775808ns"), Ok(i64::MIN));
    }
}

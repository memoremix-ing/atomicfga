//! Tuple string predicates, ported line for line from OpenFGA's pkg/tuple.
//!
//! Go ranges over strings by rune with byte offsets; Rust's `char_indices`
//! does the same. `char::is_control` and Go's `unicode.IsControl` both mean
//! general category Cc.

pub const WILDCARD: &str = "*";

pub fn contains_forbidden_chars(s: &str) -> bool {
    s.chars().any(char::is_control)
}

/// `type:id` → (type, id); without `:`, ("", s).
pub fn split_object(s: &str) -> (&str, &str) {
    match s.find(':') {
        None => ("", s),
        Some(i) => (&s[..i], &s[i + 1..]),
    }
}

/// `object#relation` → (object, relation), splitting at the last `#`.
pub fn split_object_relation(s: &str) -> (&str, &str) {
    match s.rfind('#') {
        None => (s, ""),
        Some(i) if i == s.len() - 1 => (&s[..i], ""),
        Some(i) => (&s[..i], &s[i + 1..]),
    }
}

pub fn get_type(s: &str) -> &str {
    split_object(s).0
}

pub fn get_relation(s: &str) -> &str {
    split_object_relation(s).1
}

pub fn is_valid_object(s: &str) -> bool {
    let (mut state, mut id_len) = (0usize, 0usize);
    for (ndx, chr) in s.char_indices() {
        if chr.is_control() {
            return false;
        }
        match chr {
            '#' | ' ' => return false,
            ':' => {
                if state > 0 || ndx == 0 {
                    return false;
                }
                state = 1;
            }
            _ => id_len += state,
        }
    }
    id_len > 0
}

pub fn is_valid_relation(s: &str) -> bool {
    let mut count = 0;
    for chr in s.chars() {
        if chr.is_control() {
            return false;
        }
        match chr {
            '#' | ':' | '@' | ' ' => return false,
            _ => count += 1,
        }
    }
    count > 0
}

pub fn is_valid_user_id(s: &str) -> bool {
    let mut count = 0;
    for chr in s.chars() {
        if chr.is_control() {
            return false;
        }
        match chr {
            '#' | ':' | ' ' => return false,
            _ => count += 1,
        }
    }
    count > 0
}

pub fn is_valid_userset(s: &str) -> bool {
    let (mut state, mut id_len, mut rel_len) = (0, 0, 0);
    for (ndx, chr) in s.char_indices() {
        if chr.is_control() {
            return false;
        }
        match chr {
            ':' => {
                if state > 0 || ndx == 0 {
                    return false;
                }
                state = 1;
            }
            '#' => {
                if state > 1 || id_len == 0 {
                    return false;
                }
                state = 2;
            }
            ' ' => return false,
            '*' => {
                if state > 0 {
                    return false;
                }
            }
            _ => match state {
                1 => id_len += 1,
                2 => rel_len += 1,
                _ => {}
            },
        }
    }
    rel_len > 0
}

pub fn is_object_relation(s: &str) -> bool {
    is_valid_userset(s)
}

pub fn is_valid_user(s: &str) -> bool {
    s == WILDCARD || is_valid_user_id(s) || is_valid_object(s) || is_valid_userset(s)
}

pub fn is_typed_wildcard(s: &str) -> bool {
    let (t, id) = split_object(s);
    !t.is_empty() && id == WILDCARD
}

pub fn is_wildcard(s: &str) -> bool {
    s == WILDCARD || is_typed_wildcard(s)
}

/// `tuple.GetUserTypeFromUser(u) == tuple.UserSet`.
pub fn is_userset_user_type(s: &str) -> bool {
    is_object_relation(s) || is_wildcard(s)
}

pub fn is_self_defining(object: &str, relation: &str, user: &str) -> bool {
    let (user_object, user_relation) = split_object_relation(user);
    relation == user_relation && object == user_object
}

pub fn tuple_key_string(object: &str, relation: &str, user: &str) -> String {
    let mut s = String::with_capacity(object.len() + relation.len() + user.len() + 2);
    s.push_str(object);
    s.push('#');
    s.push_str(relation);
    s.push('@');
    s.push_str(user);
    s
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn predicates() {
        assert!(is_valid_object("doc:1"));
        assert!(is_valid_object("doc:*"));
        assert!(!is_valid_object(":1"));
        assert!(!is_valid_object("doc:"));
        assert!(!is_valid_object("doc:1:2"));
        assert!(is_valid_userset("group:1#member"));
        assert!(!is_valid_userset("group:*#member"));
        assert!(!is_valid_userset("group#member")); // needs an id
        assert!(!is_valid_userset("group:1#"));
        assert!(is_valid_user("*"));
        assert!(is_valid_user("anne"));
        assert_eq!(split_object_relation("a#b#c"), ("a#b", "c"));
        assert_eq!(split_object_relation("a#"), ("a", ""));
        assert!(is_self_defining("group:1", "member", "group:1#member"));
    }
}

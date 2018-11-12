resource "test_resource" "r1" {
  required      = "b"
  optional_bool = true

  required_map {
    k1 = 2
    k2 = 3
  }
}

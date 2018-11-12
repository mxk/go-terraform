resource "test_resource" "r1" {
  required = "a"

  required_map {
    k1 = 1
  }
}

resource "test_resource" "r2" {
  required = "b"

  required_map {
    k1 = 2
  }
}

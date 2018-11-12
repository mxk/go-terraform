data "test_data_source" "d1" {
  input = "test123"
}

resource "test_resource" "r1" {
  required = "${data.test_data_source.d1.output}"

  required_map {
    k1 = 2
  }
}
